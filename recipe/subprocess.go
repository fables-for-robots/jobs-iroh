package recipe

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"reflect"

	"github.com/fxamacker/cbor/v2"
	"github.com/jobs-build/jobs-iroh/tailbuf"
)

// exTempFail (EX_TEMPFAIL) marks a retryable plugin failure (build.md §6).
const exTempFail = 75

// PluginError is a plugin's non-zero exit (build.md §6, §11). Retryable is true
// only for exit 75; any other non-zero exit is a hard (eval) failure.
type PluginError struct {
	ExitCode  int
	Retryable bool
	Stderr    string
}

func (e *PluginError) Error() string {
	class := "hard"
	if e.Retryable {
		class = "retryable"
	}
	return fmt.Sprintf("plugin exited %d (%s): %s", e.ExitCode, class, e.Stderr)
}

// pluginRequest is the CBOR stdin payload (build.md §6).
type pluginRequest struct {
	Call   map[string]any `cbor:"call"`
	Source string         `cbor:"source"`
}

// respDec decodes the plugin's CBOR response with string map keys, so nested
// maps surface as map[string]any (matching toStarlark / asInputSpec).
var respDec = func() cbor.DecMode {
	m, err := cbor.DecOptions{DefaultMapType: reflect.TypeOf(map[string]any(nil))}.DecMode()
	if err != nil {
		panic(err)
	}
	return m
}()

// SubprocessPlugin runs a plugin artifact's ./plugin entrypoint as a subprocess,
// exchanging CBOR over stdio (build.md §6). Dir is the plugin artifact root
// (CWD); SourceDir is the read-only source tree path passed in the request.
type SubprocessPlugin struct {
	Dir       string
	SourceDir string
	Ctx       context.Context // nil → context.Background()
}

var _ PluginCaller = SubprocessPlugin{}

func (p SubprocessPlugin) Call(kwargs map[string]any) (any, error) {
	reqBytes, err := cbor.Marshal(pluginRequest{Call: kwargs, Source: p.SourceDir})
	if err != nil {
		return nil, fmt.Errorf("encode plugin request: %w", err)
	}
	ctx := p.Ctx
	if ctx == nil {
		ctx = context.Background()
	}
	cmd := exec.CommandContext(ctx, "./plugin")
	cmd.Dir = p.Dir
	cmd.Stdin = bytes.NewReader(reqBytes)
	var stdout bytes.Buffer
	tail := tailbuf.New(4 << 10)
	cmd.Stdout = &stdout
	cmd.Stderr = tail

	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code := ee.ExitCode()
			return nil, &PluginError{ExitCode: code, Retryable: code == exTempFail, Stderr: tail.String()}
		}
		return nil, fmt.Errorf("run plugin: %w", err)
	}
	var v any
	if err := respDec.Unmarshal(stdout.Bytes(), &v); err != nil {
		return nil, fmt.Errorf("decode plugin response: %w", err)
	}
	return v, nil
}
