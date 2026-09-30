// Package server 用 Echo 暴露运行档管理、会话管理与历元提交的 HTTP 接口。
package server

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"

	"github.com/labstack/echo/v4"

	"raim/pkg/apierr"
	"raim/pkg/profile"
	"raim/pkg/session"
)

// Server 持有存储并注册路由。
type Server struct {
	store *session.Store
	e     *echo.Echo
}

// New 构造 HTTP 服务（store 已绑定数据目录）。
func New(store *session.Store) *Server {
	s := &Server{store: store, e: echo.New()}
	s.e.HideBanner = true
	s.e.HTTPErrorHandler = func(err error, c echo.Context) {
		he, ok := err.(*echo.HTTPError)
		if !ok {
			he = echo.NewHTTPError(http.StatusInternalServerError, map[string]string{"error": err.Error()})
		}
		_ = c.JSON(he.Code, he.Message)
	}
	s.routes()
	return s
}

// Echo 返回内部 Echo 实例（测试与 main 复用）。
func (s *Server) Echo() *echo.Echo { return s.e }

func (s *Server) routes() {
	s.e.GET("/healthz", s.health)

	api := s.e.Group("/api")
	api.GET("/profiles", s.listProfiles)
	api.POST("/profiles", s.createProfile)
	api.GET("/profiles/:name", s.getProfile)

	api.GET("/sessions", s.listSessions)
	api.POST("/sessions", s.createSession)
	api.GET("/sessions/:id", s.getSession)
	api.POST("/sessions/:id/epochs", s.appendEpoch)
	api.POST("/sessions/:id/epochs/batch", s.appendBatch)
}

func (s *Server) health(c echo.Context) error {
	return c.JSON(http.StatusOK, map[string]string{"status": "ok"})
}

// ---- profiles ----

func (s *Server) listProfiles(c echo.Context) error {
	ps, err := s.store.ListProfiles()
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, map[string]any{"profiles": ps})
}

func (s *Server) getProfile(c echo.Context) error {
	p, err := s.store.GetProfile(c.Param("name"))
	if err != nil {
		return mapErr(err)
	}
	return c.JSON(http.StatusOK, p)
}

func (s *Server) createProfile(c echo.Context) error {
	var p profile.Profile
	if err := c.Bind(&p); err != nil {
		return apierr.New("body", "请求体不是合法的运行档 JSON: "+err.Error())
	}
	if err := s.store.CreateProfile(p); err != nil {
		return mapErr(err)
	}
	return c.JSON(http.StatusCreated, p)
}

// ---- sessions ----

type createSessionReq struct {
	ID          string `json:"id"`
	ProfileName string `json:"profile_name"`
}

func (s *Server) createSession(c echo.Context) error {
	var req createSessionReq
	if err := c.Bind(&req); err != nil {
		return apierr.New("body", "请求体不是合法 JSON: "+err.Error())
	}
	if req.ProfileName == "" {
		return apierr.New("profile_name", "创建会话必须指定运行档")
	}
	id := req.ID
	if id == "" {
		id = newID()
	}
	sess, err := s.store.CreateSession(id, req.ProfileName)
	if err != nil {
		return mapErr(err)
	}
	return c.JSON(http.StatusCreated, map[string]any{
		"id": sess.ID, "profile_name": sess.ProfileName, "created_at": sess.CreatedAt,
	})
}

func (s *Server) listSessions(c echo.Context) error {
	ids, err := s.store.ListSessions()
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, map[string]any{"sessions": ids})
}

func (s *Server) getSession(c echo.Context) error {
	sess, err := s.store.GetSession(c.Param("id"))
	if err != nil {
		return mapErr(err)
	}
	return c.JSON(http.StatusOK, sessionView(sess))
}

func (s *Server) appendEpoch(c echo.Context) error {
	var in session.EpochInput
	if err := c.Bind(&in); err != nil {
		return apierr.New("body", "历元 JSON 非法: "+err.Error())
	}
	rec, err := s.store.AppendEpoch(c.Param("id"), in)
	if err != nil {
		return mapErr(err)
	}
	return c.JSON(http.StatusOK, rec)
}

type batchReq struct {
	Epochs []session.EpochInput `json:"epochs"`
}

func (s *Server) appendBatch(c echo.Context) error {
	var req batchReq
	if err := c.Bind(&req); err != nil {
		return apierr.New("body", "批量请求 JSON 非法: "+err.Error())
	}
	recs, err := s.store.AppendBatch(c.Param("id"), req.Epochs)
	if err != nil {
		return mapErr(err)
	}
	return c.JSON(http.StatusOK, map[string]any{"epochs": recs, "count": len(recs)})
}

// sessionView 给出会话状态与统计；记录可能很长，默认只回最近若干条。
func sessionView(sess *session.Session) map[string]any {
	return map[string]any{
		"id":           sess.ID,
		"profile_name": sess.ProfileName,
		"created_at":   sess.CreatedAt,
		"last_ts":      sess.LastTS,
		"state":        sess.State,
		"stats":        sess.Stats,
		"records":      sess.Records,
	}
}

// mapErr 把领域错误映射为合适的 HTTP 状态码与字段信息。
func mapErr(err error) *echo.HTTPError {
	var fe *apierr.FieldError
	if errors.As(err, &fe) {
		return echo.NewHTTPError(http.StatusBadRequest, map[string]string{
			"error": fe.Reason, "field": fe.Field,
		})
	}
	switch {
	case errors.Is(err, apierr.ErrSessionNotFound), errors.Is(err, apierr.ErrProfileNotFound):
		return echo.NewHTTPError(http.StatusNotFound, map[string]string{"error": err.Error()})
	case errors.Is(err, apierr.ErrDuplicateEpoch):
		return echo.NewHTTPError(http.StatusConflict, map[string]string{"error": err.Error()})
	case errors.Is(err, apierr.ErrStaleEpoch):
		return echo.NewHTTPError(http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
	default:
		return echo.NewHTTPError(http.StatusBadRequest, map[string]string{"error": err.Error()})
	}
}

func newID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return "sess_" + hex.EncodeToString(b[:])
}
