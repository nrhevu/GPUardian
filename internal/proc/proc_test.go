package proc

import (
	"fmt"
	"io"
	"os"
	"strings"
	"testing"

	"rocguardd/internal/model"
)

func TestWriteMessageToPipeStderrPath(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()

	path := fmt.Sprintf("/proc/%d/fd/%d", os.Getpid(), writer.Fd())
	if err := WriteMessageToStderr(model.ProcInfo{PID: os.Getpid(), StderrPath: path}, "rocguard test\n"); err != nil {
		writer.Close()
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "rocguard test") {
		t.Fatalf("message not written to pipe: %q", string(data))
	}
}
