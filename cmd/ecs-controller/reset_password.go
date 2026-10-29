package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/Kori1c/ecs-controller/internal/store"
)

// Read from standard input, not process arguments or an environment variable.
func resetPassword(dataDir string, in io.Reader, out io.Writer) error {
	if _, err := os.Stat(filepath.Join(dataDir, "data.sqlite")); err != nil {
		return fmt.Errorf("未找到已有数据库，请确认 ECS_DATA_DIR 指向当前服务的数据目录")
	}
	raw, err := io.ReadAll(io.LimitReader(in, 76))
	if err != nil {
		return fmt.Errorf("无法读取密码")
	}
	password := strings.TrimSuffix(strings.TrimSuffix(string(raw), "\n"), "\r")
	if len(password) > 72 || strings.ContainsAny(password, "\r\n") {
		return fmt.Errorf("请输入单行密码，长度需要 6 到 72 字节")
	}
	st, err := store.Open(dataDir)
	if err != nil {
		return err
	}
	defer st.Close()
	if err = st.ResetAdminPassword(password); err != nil {
		return err
	}
	fmt.Fprintln(out, "管理员密码已重置，密码登录已开启，旧登录会话已失效。账号、配置、历史记录和 Passkey 均已保留。")
	return nil
}
