package ops_test

import (
	"testing"
	"time"

	"github.com/IniZio/outpost/internal/domain"
	"github.com/IniZio/outpost/internal/ops"
)

func goodBuild() ops.SetupBuildReq {
	return ops.SetupBuildReq{
		Repo: "o/r", Rev: 2, Files: map[string]string{domain.SetupBake: `target "box" {}`}, Base: "ob-base:1",
		Profile: [][]string{{"claude", "plugin", "install", "x"}}, Keep: []int{1}, Budget: time.Hour,
	}
}

func TestSetupBuildValidateRefusesWhatCouldEscape(t *testing.T) {
	if err := goodBuild().Validate(); err != nil {
		t.Fatalf("a good request is valid: %v", err)
	}
	r := goodBuild()
	r.Commit = "0123abc"
	if err := r.Validate(); err != nil {
		t.Fatalf("a short commit is valid: %v", err)
	}
	bad := map[string]func(*ops.SetupBuildReq){
		"no revision":     func(r *ops.SetupBuildReq) { r.Rev = 0 },
		"dotdot repo":     func(r *ops.SetupBuildReq) { r.Repo = "o/.." },
		"flag base":       func(r *ops.SetupBuildReq) { r.Base = "--output=/" },
		"no budget":       func(r *ops.SetupBuildReq) { r.Budget = 0 },
		"flag commit":     func(r *ops.SetupBuildReq) { r.Commit = "--upload-pack=x" },
		"empty profile":   func(r *ops.SetupBuildReq) { r.Profile = [][]string{{}} },
		"bad keep":        func(r *ops.SetupBuildReq) { r.Keep = []int{0} },
		"no bake file":    func(r *ops.SetupBuildReq) { r.Files = map[string]string{"a": ""} },
		"a path for file": func(r *ops.SetupBuildReq) { r.Files[domain.SetupBake+"/../x"] = "" },
	}
	for name, mut := range bad {
		r := goodBuild()
		mut(&r)
		if r.Validate() == nil {
			t.Errorf("%s: must be refused", name)
		}
	}
	if ops.ValidInner("") != nil || ops.ValidInner("0123456789abcdef") != nil || ops.ValidInner("XYZ") == nil {
		t.Error("inner keys are empty or hex")
	}
}
