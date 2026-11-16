package claude

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"strings"
	"time"

	"github.com/IniZio/outpost/internal/ports"
)

// LastMessage reads the last assistant message of the task's session. It looks again every Poll
// until it finds one or ctx is done, and then returns "" with no error. A message left over from
// an earlier turn counts: the caller decides when the agent has settled.
func (a *Agent) LastMessage(ctx context.Context, bfs ports.BoxFS, t ports.Target) (string, error) {
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return "", nil
		case <-timer.C:
		}
		msg, err := readLast(ctx, bfs, t)
		if err != nil || msg != "" {
			return msg, err
		}
		timer.Reset(a.cfg.Poll)
	}
}

// readLast is one look. A missing or half-written file is not an error, only not yet.
func readLast(ctx context.Context, bfs ports.BoxFS, t ports.Target) (string, error) {
	raw, err := bfs.ReadFile(ctx, t, path.Join(Home, sessionPointer))
	if err != nil {
		return "", missingOK(err)
	}
	var ptr struct {
		Transcript string `json:"transcript_path"`
	}
	if json.Unmarshal(raw, &ptr) != nil || ptr.Transcript == "" {
		return "", nil
	}
	data, err := bfs.ReadFile(ctx, t, ptr.Transcript)
	if err != nil {
		return "", missingOK(err)
	}
	return lastText(data), nil
}

func missingOK(err error) error {
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return fmt.Errorf("claude: read session: %w", err)
}

// entry is the part of a transcript line that carries a message.
type entry struct {
	Type      string `json:"type"`
	Sidechain bool   `json:"isSidechain"`
	Message   struct {
		Model   string `json:"model"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	} `json:"message"`
}

// text is the entry's last text block, or "" for a line that is no assistant answer. Claude Code
// writes its own error lines ("Please run /login") as assistant lines of the model "<synthetic>";
// they are not the agent's words.
func (e entry) text() string {
	if e.Type != "assistant" || e.Sidechain || e.Message.Model == "<synthetic>" {
		return ""
	}
	for i := len(e.Message.Content) - 1; i >= 0; i-- {
		if c := e.Message.Content[i]; c.Type == "text" && strings.TrimSpace(c.Text) != "" {
			return strings.TrimSpace(c.Text)
		}
	}
	return ""
}

// lastText scans the JSONL from the end, so a long session costs only its tail. The last line may
// be half written, and a line that does not parse is skipped.
func lastText(data []byte) string {
	for end := len(data); end > 0; {
		start := bytes.LastIndexByte(data[:end], '\n') + 1
		line := data[start:end]
		end = max(start-1, 0)
		if !bytes.Contains(line, []byte(`"assistant"`)) {
			continue
		}
		var e entry
		if json.Unmarshal(line, &e) == nil {
			if s := e.text(); s != "" {
				return s
			}
		}
	}
	return ""
}
