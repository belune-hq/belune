package crowdsec

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/belune-hq/belune/internal/runtime"
)

const capiJournal = "/c/.belune-capi-rollback"

// A helper on the actual config volume remains executable while CrowdSec
// crash-loops. Its image is the inspected image ID, so recovery needs no pull.
type capiFiles struct {
	rt runtime.ContainerRuntime
	id string
}

func newCAPIFiles(ctx context.Context, rt runtime.ContainerRuntime, name string) (*capiFiles, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	storage, err := rt.ContainerStorage(ctx, name)
	if err != nil {
		return nil, err
	}
	volume := ""
	for _, mount := range storage.Mounts {
		if strings.HasPrefix(mount.Destination, "/etc/crowdsec/") {
			return nil, errors.New("CrowdSec config has nested mounts that the recovery helper cannot safely restore")
		}
		if mount.Destination != "/etc/crowdsec" {
			continue
		}
		if mount.Type != "volume" || !mount.Writable || mount.Name == "" {
			return nil, errors.New("CrowdSec config must be on a writable Docker volume before CAPI can be changed")
		}
		volume = mount.Name
	}
	if volume == "" || storage.Image == "" {
		return nil, errors.New("could not identify the CrowdSec config volume and cached image")
	}
	id, err := rt.CreateContainer(ctx, runtime.ContainerConfig{
		Name: "belune-capi-helper-" + rand.Text(), Image: storage.Image, AutoRemove: true,
		Entrypoint: []string{"sh"}, Cmd: []string{"-c", "sleep 900"},
		Volumes: map[string]string{volume: "/c"}, Network: "none",
		ReadonlyRootfs: true, Tmpfs: map[string]string{"/tmp": ""},
		Labels: map[string]string{"belune-system": "capi-helper"},
	})
	if err != nil {
		return nil, fmt.Errorf("create CAPI recovery helper: %w", err)
	}
	f := &capiFiles{rt: rt, id: id}
	if err := rt.StartContainer(ctx, id); err != nil {
		f.close(ctx)
		return nil, fmt.Errorf("start CAPI recovery helper: %w", err)
	}
	return f, nil
}

func (f *capiFiles) close(ctx context.Context) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	_ = f.rt.RemoveContainer(ctx, f.id)
}

func (f *capiFiles) run(ctx context.Context, script string, input io.Reader) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var out, stderr limitedBuffer
	code, err := f.rt.ContainerExec(ctx, f.id, []string{"sh", "-ec", script}, input, &out, &stderr)
	if err != nil {
		return "", fmt.Errorf("CAPI volume operation: %w", err)
	}
	if code != 0 {
		return "", fmt.Errorf("CAPI volume operation failed (exit %d)", code)
	}
	return out.String(), nil
}

func (f *capiFiles) read(ctx context.Context, name string) ([]byte, error) {
	out, err := f.run(ctx, `test ! -L /c/`+name+`; if [ -f /c/`+name+` ]; then test "$(wc -c < /c/`+name+`)" -le 1048576; cat /c/`+name+`; fi`, nil)
	return []byte(out), err
}

func (f *capiFiles) hasJournal(ctx context.Context) (bool, error) {
	out, err := f.run(ctx, `test ! -L `+capiJournal+`; if [ -d `+capiJournal+` ]; then printf yes; fi`, nil)
	return out == "yes", err
}

func (f *capiFiles) snapshot(ctx context.Context) error {
	_, err := f.run(ctx, `umask 077; mkdir /tmp/capi-before
for name in config.yaml.local online_api_credentials.yaml console.yaml; do
 test ! -L "/c/$name"
 if [ -e "/c/$name" ]; then
  test -f "/c/$name"; test "$(wc -c < "/c/$name")" -le 1048576
  cp -p "/c/$name" "/tmp/capi-before/$name"
 else touch "/tmp/capi-before/$name.absent"; fi
done`, nil)
	return err
}

func (f *capiFiles) begin(ctx context.Context) error {
	_, err := f.run(ctx, `test ! -e `+capiJournal+`; umask 077; stage=$(mktemp -d /c/.belune-capi-snapshot.XXXXXX); trap 'rm -rf "$stage"' EXIT HUP INT TERM; cp -a /tmp/capi-before/. "$stage"/; mv "$stage" `+capiJournal, nil)
	return err
}

func (f *capiFiles) restore(ctx context.Context) error {
	_, err := f.run(ctx, `test -d `+capiJournal+`; test ! -L `+capiJournal+`
for name in config.yaml.local online_api_credentials.yaml console.yaml; do
 test ! -L "/c/$name"
 if [ -f "`+capiJournal+`/$name.absent" ]; then rm -f "/c/$name"
 else cp -p "`+capiJournal+`/$name" "/c/.belune-capi-restore"; mv -f /c/.belune-capi-restore "/c/$name"; fi
done`, nil)
	return err
}

func (f *capiFiles) commit(ctx context.Context) error {
	_, err := f.run(ctx, `test ! -L `+capiJournal+`; rm -rf `+capiJournal, nil)
	return err
}

// Paths are internal fixed names, never request strings. Atomic replacement
// prevents a restart observing a partially written credential or override.
func (f *capiFiles) write(ctx context.Context, name string, content []byte) error {
	if name != "config.yaml.local" && name != "online_api_credentials.yaml" {
		return errors.New("unsupported CAPI file")
	}
	_, err := f.run(ctx, `umask 077; test ! -L /c/`+name+`; temp=$(mktemp /c/.belune-capi-write.XXXXXX); trap 'rm -f "$temp"' EXIT HUP INT TERM; cat > "$temp"; chmod 600 "$temp"; mv -f "$temp" /c/`+name, strings.NewReader(string(content)))
	return err
}
