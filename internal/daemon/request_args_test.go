package daemon

import (
	"strings"
	"testing"
)

func TestDecodeRegisterArgsRejectsAmplifyingGPUArray(t *testing.T) {
	raw := []byte(`{"gpus":[` + strings.Repeat(`0,`, maxGPUsPerRequest) + `0]}`)
	if _, err := decodeRegisterArgs(raw); err == nil {
		t.Fatal("oversized GPU array unexpectedly decoded")
	}
}

func TestDecodeRunArgsRejectsAmplifyingArrays(t *testing.T) {
	tests := []struct {
		name string
		raw  string
	}{
		{
			name: "command entries",
			raw:  `{"command":[` + strings.Repeat(`"",`, maxRunCommandArgs) + `""]}`,
		},
		{
			name: "command bytes",
			raw:  `{"command":["` + strings.Repeat("x", maxRunCommandBytes+1) + `"]}`,
		},
		{
			name: "environment entries",
			raw:  `{"command":["true"],"env":[` + strings.Repeat(`"",`, maxRunEnvEntries) + `""]}`,
		},
		{
			name: "environment bytes",
			raw:  `{"command":["true"],"env":["` + strings.Repeat("x", maxRunEnvBytes+1) + `"]}`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := decodeRunArgs([]byte(test.raw)); err == nil {
				t.Fatal("oversized run arguments unexpectedly decoded")
			}
		})
	}
}

func TestDecodeRunArgsAcceptsBoundedRequest(t *testing.T) {
	args, err := decodeRunArgs([]byte(`{"name":"GLM TP4 benchmark","command":["sh","-c","true"],"workdir":"/tmp","env":["PATH=/bin"]}`))
	if err != nil {
		t.Fatal(err)
	}
	if args.Name != "GLM TP4 benchmark" || len(args.Command) != 3 || args.Workdir != "/tmp" || len(args.Env) != 1 {
		t.Fatalf("unexpected decoded arguments: %+v", args)
	}
}
