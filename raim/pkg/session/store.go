package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"raim/pkg/apierr"
	"raim/pkg/profile"
	"raim/pkg/statem"
)

// Store 把运行档与会话状态存到本地文件：
//
//	<dir>/profiles/<name>.json
//	<dir>/sessions/<id>.json
//
// 写入采用“临时文件 + rename”原子替换，避免半写损坏。
type Store struct {
	dir string
	mu  sync.Mutex
}

// NewStore 打开（必要时创建）数据目录，并把缺失的内置运行档落盘。
func NewStore(dir string) (*Store, error) {
	if err := os.MkdirAll(filepath.Join(dir, "profiles"), 0o755); err != nil {
		return nil, fmt.Errorf("创建 profiles 目录: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "sessions"), 0o755); err != nil {
		return nil, fmt.Errorf("创建 sessions 目录: %w", err)
	}
	st := &Store{dir: dir}
	for _, p := range profile.Builtins() {
		path := st.profilePath(p.Name)
		if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
			if err := st.writeJSON(path, p); err != nil {
				return nil, err
			}
		}
	}
	return st, nil
}

// Dir 返回数据目录。
func (st *Store) Dir() string { return st.dir }

// ---- profiles ----

func (st *Store) profilePath(name string) string {
	return filepath.Join(st.dir, "profiles", sanitize(name)+".json")
}

// ListProfiles 列出全部运行档（内置 + 用户新建）。
func (st *Store) ListProfiles() ([]profile.Profile, error) {
	files, err := os.ReadDir(filepath.Join(st.dir, "profiles"))
	if err != nil {
		return nil, err
	}
	var out []profile.Profile
	for _, f := range files {
		if f.IsDir() || !strings.HasSuffix(f.Name(), ".json") {
			continue
		}
		var p profile.Profile
		if err := readJSON(filepath.Join(st.dir, "profiles", f.Name()), &p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// GetProfile 按名读取运行档。
func (st *Store) GetProfile(name string) (profile.Profile, error) {
	var p profile.Profile
	if err := readJSON(st.profilePath(name), &p); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return p, apierr.ErrProfileNotFound
		}
		return p, err
	}
	return p, nil
}

// CreateProfile 校验并新建运行档；重名返回字段错误。
func (st *Store) CreateProfile(p profile.Profile) error {
	if err := p.Validate(); err != nil {
		return err
	}
	path := st.profilePath(p.Name)
	st.mu.Lock()
	defer st.mu.Unlock()
	if _, err := os.Stat(path); err == nil {
		return apierr.New("name", "运行档已存在")
	}
	return st.writeJSON(path, p)
}

// ---- sessions ----

func (st *Store) sessionPath(id string) string {
	return filepath.Join(st.dir, "sessions", sanitize(id)+".json")
}

// CreateSession 新建并持久化一个绑定运行档的会话。
func (st *Store) CreateSession(id, profileName string) (*Session, error) {
	// 先确认运行档存在
	if _, err := st.GetProfile(profileName); err != nil {
		return nil, err
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	path := st.sessionPath(id)
	if _, err := os.Stat(path); err == nil {
		return nil, apierr.New("id", "会话已存在")
	}
	profiles, err := st.ListProfiles()
	if err != nil {
		return nil, err
	}
	svc := NewService(profiles)
	sess, err := svc.NewSession(id, profileName)
	if err != nil {
		return nil, err
	}
	if err := st.writeJSON(path, sess); err != nil {
		return nil, err
	}
	return sess, nil
}

// GetSession 读取会话（重启后续跑）。
func (st *Store) GetSession(id string) (*Session, error) {
	var sess Session
	if err := readJSON(st.sessionPath(id), &sess); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, apierr.ErrSessionNotFound
		}
		return nil, err
	}
	if sess.State.Isolated == nil {
		sess.State.Isolated = map[int]*statem.IsolationEntry{}
	}
	return &sess, nil
}

// ListSessions 列出全部会话 ID。
func (st *Store) ListSessions() ([]string, error) {
	files, err := os.ReadDir(filepath.Join(st.dir, "sessions"))
	if err != nil {
		return nil, err
	}
	var out []string
	for _, f := range files {
		if !f.IsDir() && strings.HasSuffix(f.Name(), ".json") {
			out = append(out, strings.TrimSuffix(f.Name(), ".json"))
		}
	}
	sort.Strings(out)
	return out, nil
}

// AppendEpoch 在锁内读取-推进-写回，返回该历元记录。
// 重复/倒退时间戳返回哨兵错误且不落盘（状态不推进）。
func (st *Store) AppendEpoch(id string, in EpochInput) (*EpochRecord, error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	sess, err := st.loadLocked(id)
	if err != nil {
		return nil, err
	}
	profiles, err := st.ListProfiles()
	if err != nil {
		return nil, err
	}
	svc := NewService(profiles)
	rec, err := svc.Step(sess, in)
	if err != nil {
		return nil, err
	}
	if err := st.writeJSON(st.sessionPath(id), sess); err != nil {
		return nil, err
	}
	return rec, nil
}

// AppendBatch 一次提交整段历元（至多 3600 个），整段原子推进。
// 若中间任一时间戳重复/倒退或数据非法，整段拒收、不写状态。
func (st *Store) AppendBatch(id string, ins []EpochInput) ([]*EpochRecord, error) {
	const maxBatch = 3600
	if len(ins) == 0 {
		return nil, apierr.New("epochs", "批量提交为空")
	}
	if len(ins) > maxBatch {
		return nil, apierr.New("epochs",
			fmt.Sprintf("一次最多提交 %d 个历元，本次 %d 个", maxBatch, len(ins)))
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	sess, err := st.loadLocked(id)
	if err != nil {
		return nil, err
	}
	profiles, err := st.ListProfiles()
	if err != nil {
		return nil, err
	}
	svc := NewService(profiles)

	// 先在副本上整段试跑，任一历元失败则整体不落地
	snap := *sess
	snap.Records = append([]*EpochRecord(nil), sess.Records...)
	snap.State = cloneState(sess.State)
	recs := make([]*EpochRecord, 0, len(ins))
	for i, in := range ins {
		rec, err := svc.Step(&snap, in)
		if err != nil {
			return nil, prefixIndex(err, i)
		}
		recs = append(recs, rec)
	}
	if err := st.writeJSON(st.sessionPath(id), &snap); err != nil {
		return nil, err
	}
	return recs, nil
}

func (st *Store) loadLocked(id string) (*Session, error) {
	sess, err := st.GetSession(id)
	if err != nil {
		return nil, err
	}
	return sess, nil
}

// ---- json io ----

func (st *Store) writeJSON(path string, v any) error {
	tmp := path + ".tmp"
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

func sanitize(name string) string {
	r := strings.NewReplacer("/", "_", "\\", "_", "..", "_", " ", "_")
	return r.Replace(name)
}

// cloneState 深拷贝跨历元状态（Isolated 是指针 map，必须逐元素复制）。
func cloneState(s statem.State) statem.State {
	out := statem.State{
		Alert:    s.Alert,
		Isolated: map[int]*statem.IsolationEntry{},
	}
	for id, e := range s.Isolated {
		cp := *e
		out.Isolated[id] = &cp
	}
	return out
}

// prefixIndex 给批量中的字段错误加上历元下标，便于定位。
func prefixIndex(err error, i int) error {
	var fe *apierr.FieldError
	if errors.As(err, &fe) {
		return apierr.New(fmt.Sprintf("epochs[%d].%s", i, fe.Field), fe.Reason)
	}
	return err
}
