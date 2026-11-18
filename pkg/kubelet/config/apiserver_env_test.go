package config

import "testing"

func TestEnvHelpers(t *testing.T) {
	t.Setenv("RT_TEST_INT", "4")
	t.Setenv("RT_TEST_BAD", "x")
	t.Setenv("RT_TEST_BOOL", "false")
	t.Setenv("RT_TEST_EMPTY", "")

	if got := envInt("RT_TEST_INT", 8); got != 4 {
		t.Errorf("envInt set = %d, want 4", got)
	}
	if got := envInt("RT_TEST_BAD", 8); got != 8 {
		t.Errorf("envInt invalid = %d, want default 8", got)
	}
	if got := envInt("RT_TEST_UNSET", 8); got != 8 {
		t.Errorf("envInt unset = %d, want default 8", got)
	}
	if got := envBool("RT_TEST_BOOL", true); got {
		t.Errorf("envBool set = %v, want false", got)
	}
	if got := envBool("RT_TEST_BAD", true); !got {
		t.Errorf("envBool invalid = %v, want default true", got)
	}
	if got := envString("RT_TEST_EMPTY", "test-priority"); got != "" {
		t.Errorf("envString empty = %q, want empty", got)
	}
	if got := envString("RT_TEST_UNSET", "test-priority"); got != "test-priority" {
		t.Errorf("envString unset = %q, want default", got)
	}
}
