package main

import (
	"bytes"
	"github.com/Kori1c/ecs-controller/internal/store"
	"strings"
	"testing"
)

func TestResetPasswordCLIUsesStdinWithoutPrintingSecret(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	st.SetAdminPassword("old-password")
	st.Close()
	var out bytes.Buffer
	if err = resetPassword(dir, strings.NewReader("new-secret-password\n"), &out); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "new-secret-password") {
		t.Fatal("CLI printed secret")
	}
	st, err = store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if !st.CheckAdminPassword("new-secret-password") {
		t.Fatal("CLI did not persist password")
	}
	for _, input := range []string{"short", strings.Repeat("x", 73), "first-password\nsecond-password\n"} {
		if err = resetPassword(dir, strings.NewReader(input), &out); err == nil {
			t.Fatal("invalid stdin accepted")
		}
	}
	if err = resetPassword(t.TempDir(), strings.NewReader("new-password"), &out); err == nil {
		t.Fatal("reset initialized absent database")
	}
}
