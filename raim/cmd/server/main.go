// Command server 启动机载 GNSS 完好性监测回放服务。
//
// 运行档与会话文件落在 --data 指向的挂载目录；默认 /data。
package main

import (
	"flag"
	"log"

	"raim/pkg/server"
	"raim/pkg/session"
)

func main() {
	addr := flag.String("addr", ":8080", "监听地址")
	dataDir := flag.String("data", "/data", "运行档与会话文件目录（应挂载持久卷）")
	flag.Parse()

	store, err := session.NewStore(*dataDir)
	if err != nil {
		log.Fatalf("初始化数据目录 %s 失败: %v", *dataDir, err)
	}
	srv := server.New(store)
	log.Printf("RAIM 回放服务监听 %s，数据目录 %s", *addr, *dataDir)
	if err := srv.Echo().Start(*addr); err != nil {
		log.Fatal(err)
	}
}
