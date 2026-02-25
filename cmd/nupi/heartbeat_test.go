package main

import (
	"strings"
	"testing"
)

func TestNewHeartbeatCommandStructure(t *testing.T) {
	cmd := newHeartbeatCommand()
	if cmd.Use != "heartbeat" {
		t.Fatalf("expected Use='heartbeat', got %q", cmd.Use)
	}

	// Verify subcommands exist
	subNames := make(map[string]bool)
	for _, sub := range cmd.Commands() {
		subNames[sub.Name()] = true
	}
	for _, expected := range []string{"list", "add", "remove"} {
		if !subNames[expected] {
			t.Fatalf("expected subcommand %q, got %v", expected, subNames)
		}
	}
}

func TestHeartbeatAddArgsValidation(t *testing.T) {
	cmd := newHeartbeatCommand()

	// "add" requires exactly 3 args — verify cobra rejects fewer
	addCmd, _, err := cmd.Find([]string{"add"})
	if err != nil {
		t.Fatal(err)
	}

	// 0 args should fail
	if err := addCmd.Args(addCmd, nil); err == nil {
		t.Fatal("expected error for 0 args")
	}

	// 2 args should fail
	if err := addCmd.Args(addCmd, []string{"name", "cron"}); err == nil {
		t.Fatal("expected error for 2 args")
	}

	// 3 args should pass
	if err := addCmd.Args(addCmd, []string{"name", "cron", "prompt"}); err != nil {
		t.Fatalf("expected 3 args to pass, got %v", err)
	}
}

func TestHeartbeatRemoveArgsValidation(t *testing.T) {
	cmd := newHeartbeatCommand()

	removeCmd, _, err := cmd.Find([]string{"remove"})
	if err != nil {
		t.Fatal(err)
	}

	// 0 args should fail
	if err := removeCmd.Args(removeCmd, nil); err == nil {
		t.Fatal("expected error for 0 args")
	}

	// 1 arg should pass
	if err := removeCmd.Args(removeCmd, []string{"name"}); err != nil {
		t.Fatalf("expected 1 arg to pass, got %v", err)
	}

	// 2 args should fail
	if err := removeCmd.Args(removeCmd, []string{"a", "b"}); err == nil {
		t.Fatal("expected error for 2 args")
	}
}

func TestHeartbeatAddValidation(t *testing.T) {
	t.Run("empty name after trim", func(t *testing.T) {
		cmd := newHeartbeatCommand()
		addCmd, _, _ := cmd.Find([]string{"add"})
		addCmd.Flags().Bool("json", false, "")

		err := heartbeatAdd(addCmd, []string{"   ", "* * * * *", "hello"})
		if err == nil {
			t.Fatal("expected error for empty name")
		}
	})

	t.Run("empty prompt after trim", func(t *testing.T) {
		cmd := newHeartbeatCommand()
		addCmd, _, _ := cmd.Find([]string{"add"})
		addCmd.Flags().Bool("json", false, "")

		err := heartbeatAdd(addCmd, []string{"test", "* * * * *", "   "})
		if err == nil {
			t.Fatal("expected error for empty prompt")
		}
	})

	t.Run("empty cron_expr after trim", func(t *testing.T) {
		cmd := newHeartbeatCommand()
		addCmd, _, _ := cmd.Find([]string{"add"})
		addCmd.Flags().Bool("json", false, "")

		err := heartbeatAdd(addCmd, []string{"test", "   ", "hello"})
		if err == nil {
			t.Fatal("expected error for empty cron_expr")
		}
		if !strings.Contains(err.Error(), "cannot be empty") {
			t.Fatalf("expected empty cron_expr error, got %v", err)
		}
	})

	t.Run("invalid cron expression", func(t *testing.T) {
		cmd := newHeartbeatCommand()
		addCmd, _, _ := cmd.Find([]string{"add"})
		addCmd.Flags().Bool("json", false, "")

		err := heartbeatAdd(addCmd, []string{"test", "not-a-cron", "hello"})
		if err == nil {
			t.Fatal("expected error for invalid cron expression")
		}
		if !strings.Contains(err.Error(), "cron expression") {
			t.Fatalf("expected cron error message, got %v", err)
		}
	})

	t.Run("name with control characters", func(t *testing.T) {
		cmd := newHeartbeatCommand()
		addCmd, _, _ := cmd.Find([]string{"add"})
		addCmd.Flags().Bool("json", false, "")

		err := heartbeatAdd(addCmd, []string{"bad\tname", "* * * * *", "hello"})
		if err == nil {
			t.Fatal("expected error for name with control characters")
		}
		if !strings.Contains(err.Error(), "control characters") {
			t.Fatalf("expected control characters error, got %v", err)
		}
	})

	t.Run("name with double quotes", func(t *testing.T) {
		cmd := newHeartbeatCommand()
		addCmd, _, _ := cmd.Find([]string{"add"})
		addCmd.Flags().Bool("json", false, "")

		err := heartbeatAdd(addCmd, []string{`has"quote`, "* * * * *", "hello"})
		if err == nil {
			t.Fatal("expected error for name with double quotes")
		}
		if !strings.Contains(err.Error(), "double quotes") {
			t.Fatalf("expected double quotes error, got %v", err)
		}
	})

	t.Run("name exceeds max length", func(t *testing.T) {
		cmd := newHeartbeatCommand()
		addCmd, _, _ := cmd.Find([]string{"add"})
		addCmd.Flags().Bool("json", false, "")

		longName := strings.Repeat("x", 256)
		err := heartbeatAdd(addCmd, []string{longName, "* * * * *", "hello"})
		if err == nil {
			t.Fatal("expected error for name exceeding max length")
		}
		if !strings.Contains(err.Error(), "maximum length") {
			t.Fatalf("expected max length error, got %v", err)
		}
	})

	t.Run("prompt exceeds max length", func(t *testing.T) {
		cmd := newHeartbeatCommand()
		addCmd, _, _ := cmd.Find([]string{"add"})
		addCmd.Flags().Bool("json", false, "")

		longPrompt := strings.Repeat("x", 10001)
		err := heartbeatAdd(addCmd, []string{"test", "* * * * *", longPrompt})
		if err == nil {
			t.Fatal("expected error for prompt exceeding max length")
		}
		if !strings.Contains(err.Error(), "maximum length") {
			t.Fatalf("expected max length error, got %v", err)
		}
	})

	t.Run("cron_expr exceeds max length", func(t *testing.T) {
		cmd := newHeartbeatCommand()
		addCmd, _, _ := cmd.Find([]string{"add"})
		addCmd.Flags().Bool("json", false, "")

		longCron := strings.Repeat("* ", 200)
		err := heartbeatAdd(addCmd, []string{"test", longCron, "hello"})
		if err == nil {
			t.Fatal("expected error for cron_expr exceeding max length")
		}
		if !strings.Contains(err.Error(), "maximum length") {
			t.Fatalf("expected max length error, got %v", err)
		}
	})
}

func TestHeartbeatRemoveValidation(t *testing.T) {
	t.Run("empty name after trim", func(t *testing.T) {
		cmd := newHeartbeatCommand()
		removeCmd, _, _ := cmd.Find([]string{"remove"})
		removeCmd.Flags().Bool("json", false, "")

		err := heartbeatRemove(removeCmd, []string{"   "})
		if err == nil {
			t.Fatal("expected error for empty name")
		}
	})

	t.Run("name with control characters", func(t *testing.T) {
		cmd := newHeartbeatCommand()
		removeCmd, _, _ := cmd.Find([]string{"remove"})
		removeCmd.Flags().Bool("json", false, "")

		err := heartbeatRemove(removeCmd, []string{"bad\tname"})
		if err == nil {
			t.Fatal("expected error for name with control characters")
		}
		if !strings.Contains(err.Error(), "control characters") {
			t.Fatalf("expected control characters error, got %v", err)
		}
	})

	t.Run("name with double quotes", func(t *testing.T) {
		cmd := newHeartbeatCommand()
		removeCmd, _, _ := cmd.Find([]string{"remove"})
		removeCmd.Flags().Bool("json", false, "")

		err := heartbeatRemove(removeCmd, []string{`has"quote`})
		if err == nil {
			t.Fatal("expected error for name with double quotes")
		}
		if !strings.Contains(err.Error(), "double quotes") {
			t.Fatalf("expected double quotes error, got %v", err)
		}
	})
}
