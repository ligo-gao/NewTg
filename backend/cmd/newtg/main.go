// newtg 单二进制入口：healthcheck 子命令 + RUN_MODE 三角色分派。
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"newtg/gateway/internal/config"
	"newtg/gateway/internal/platform"
)

// resolveRole 解析运行角色：env 非空优先；否则取 args[0]（即 os.Args[1]）；仍空默认 gateway。
// 不纠正拼写，保留原值交给调用方处理。
func resolveRole(env string, args []string) string {
	if env != "" {
		return env
	}
	if len(args) > 0 {
		return args[0]
	}
	return "gateway"
}

func runHealthcheck(url string) int {
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		fmt.Printf("healthcheck %s: connection failed: %v\n", url, err)
		return 1
	}
	// 健康检查是只读 GET；响应体关闭失败不影响探测结论。
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		fmt.Printf("healthcheck %s: status %s\n", url, resp.Status)
		return 1
	}
	fmt.Printf("healthcheck %s: %s\n", url, resp.Status)
	return 0
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)

	// 子命令优先：healthcheck --url <url>
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		fs := flag.NewFlagSet("healthcheck", flag.ExitOnError)
		url := fs.String("url", "http://localhost:8080/healthz", "health check URL")
		// flag.ExitOnError 模式下解析失败已直接退出进程，返回值无需再处理。
		_ = fs.Parse(os.Args[2:])
		os.Exit(runHealthcheck(*url))
	}

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config load failed: %v", err)
	}

	role := resolveRole(os.Getenv("RUN_MODE"), os.Args[1:])

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	var runErr error
	switch role {
	case "gateway":
		runErr = platform.RunGateway(ctx, cfg)
	case "worker":
		runErr = platform.RunWorker(ctx, cfg)
	case "migrate":
		if err := platform.RunMigrate(ctx, cfg); err != nil {
			log.Fatalf("migrate failed: %v", err)
		}
		os.Exit(0)
	default:
		log.Fatalf("unknown RUN_MODE %q", role)
	}
	if runErr != nil {
		log.Fatalf("%s exited with error: %v", role, runErr)
	}
}
