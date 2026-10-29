package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/Kori1c/ecs-controller/internal/app"
	"github.com/Kori1c/ecs-controller/internal/cloud"
	"github.com/Kori1c/ecs-controller/internal/server"
	"github.com/Kori1c/ecs-controller/internal/store"
	"github.com/Kori1c/ecs-controller/internal/worker"
)

func main() {
	log.SetFlags(log.LstdFlags | log.LUTC)
	dataDir := env("ECS_DATA_DIR", "/var/lib/ecs-controller")
	if len(os.Args) > 1 {
		if len(os.Args) != 2 || os.Args[1] != "reset-password" {
			log.Fatal("用法：ecs-controller reset-password（从标准输入读取新密码）")
		}
		if err := resetPassword(dataDir, os.Stdin, os.Stdout); err != nil {
			log.Fatal(err)
		}
		return
	}
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		log.Fatal(err)
	}
	st, err := store.Open(dataDir)
	if err != nil {
		log.Fatal(err)
	}
	defer st.Close()

	client := buildCloudClient()
	root := env("ECS_APP_DIR", ".")
	srv := server.New(st, dataDir, filepath.Join(root, "template.html"), os.Getenv("ECS_SETUP_TOKEN"), client)
	srv.CookieSecure = env("ECS_COOKIE_SECURE", "0") == "1" || strings.EqualFold(env("ECS_COOKIE_SECURE", "0"), "true")
	srv.UpdateDir = env("ECS_UPDATE_DIR", "")
	srv.CloudFactory = func(group app.Account) cloud.Client {
		return cloud.NewRPCService(group.AccessKeyID, group.AccessKeySecret, group.RegionID)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	w := &worker.Worker{Store: st, Cloud: client, CloudFactory: func(group app.AccountGroup) cloud.Client {
		return cloud.NewRPCService(group.AccessKeyID, group.AccessKeySecret, group.RegionID)
	}, Log: log.Default()}
	go w.Run(ctx)
	go w.Monitor(ctx, time.Duration(numberEnv("ECS_MONITOR_INTERVAL", 60))*time.Second)
	go w.TelegramControl(ctx)

	addr := env("ECS_HTTP_ADDR", ":8080")
	httpServer := &http.Server{Addr: addr, Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 60 * time.Second, IdleTimeout: 120 * time.Second, TLSConfig: &tls.Config{MinVersion: tls.VersionTLS12}}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdownCtx)
	}()
	log.Printf("ecs-controller listening on %s", addr)
	if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}

func buildCloudClient() cloud.Client {
	ak, secret := os.Getenv("ALIYUN_ACCESS_KEY_ID"), os.Getenv("ALIYUN_ACCESS_KEY_SECRET")
	region := env("ALIYUN_REGION_ID", "cn-hongkong")
	if ak == "" || secret == "" {
		log.Printf("阿里云凭据未通过环境变量配置；请在账号组中保存凭据并接入凭据工厂后启用真实云操作")
		return nil
	}
	return cloud.NewRPCService(ak, secret, region)
}

func env(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}
func numberEnv(key string, fallback int) int {
	v := os.Getenv(key)
	n, err := fmtSscanf(v, "%d")
	if err != nil || n < 1 {
		return fallback
	}
	return n
}

func fmtSscanf(s, format string) (int, error) {
	var n int
	_, err := fmt.Sscanf(s, format, &n)
	return n, err
}
