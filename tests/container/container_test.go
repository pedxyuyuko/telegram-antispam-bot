// These tests exercise a built image against local HTTP fixtures. Enable with
// ANTISPAM_CONTAINER_IMAGE and optionally CONTAINER_ENGINE=docker or podman.
package container_test

import (
	"bytes"
	"context"
	"crypto/x509"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	stdimage "image"
	_ "image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	_ "modernc.org/sqlite"
	"telegram-antispam-bot/internal/config"
)

func image(t *testing.T) (string, string) {
	t.Helper()
	name := os.Getenv("ANTISPAM_CONTAINER_IMAGE")
	if name == "" {
		t.Skip("set ANTISPAM_CONTAINER_IMAGE to run container integration tests")
	}
	engine := os.Getenv("CONTAINER_ENGINE")
	if engine == "" {
		engine = "docker"
	}
	command(t, engine, "image", "inspect", name)
	return engine, name
}

func command(t *testing.T, engine string, args ...string) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	cmd := containerCommand(ctx, engine, args...)
	var out, stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("%s %v: %v\n%s\n%s", engine, args, err, stderr.String(), out.String())
	}
	return out.Bytes()
}

func containerCommand(ctx context.Context, engine string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, engine, args...)
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		switch key {
		case "TELEGRAM_BOT_TOKEN", "SYSTEMONE_API_KEY", "BOT_UID", "BOT_GID", "ANTISPAM_IMAGE", "COMPOSE_FILE", "COMPOSE_PROJECT_NAME", "COMPOSE_ENV_FILES":
			continue
		}
		cmd.Env = append(cmd.Env, entry)
	}
	return cmd
}

func cleanupContainer(t *testing.T, engine, name string) {
	t.Helper()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_ = containerCommand(ctx, engine, "rm", "--force", name).Run()
	})
}

func TestImageHasNonRootDefaultsAndHTTPSRoots(t *testing.T) {
	engine, name := image(t)
	user := strings.TrimSpace(string(command(t, engine, "image", "inspect", "--format", "{{.Config.User}}", name)))
	if user != "65532:65532" {
		t.Fatalf("image default user=%q", user)
	}
	raw := command(t, engine, "run", "--rm", "--pull=never", "--network=none", "--read-only", name, "-example")
	c, err := config.Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	if c.Base.Mode != "record" {
		t.Fatal("image example enables automatic punishment")
	}
	join := c.ForJoinGroup(-100)
	joinQuestions, err := c.SelectedQuestions(join.Policy)
	if c.JoinGate == nil || join.Enabled || join.Mode != "record" || err != nil || len(joinQuestions) != 5 {
		t.Fatal("image example lost the disabled independent join gate")
	}
	if r := join.Rules["delete_join_message"]; !r.Enabled || r.Action != "delete" {
		t.Fatal("image example lost join-message cleanup")
	}
	if c.Base.Scoring == nil || c.Base.Condition == "" || len(c.QuestionGroups) != 4 {
		t.Fatal("image example lost the grouped scoring configuration")
	}
	questions, err := c.SelectedQuestions(c.Base)
	if err != nil || len(questions) != 17 {
		t.Fatalf("image example lost the seventeen-question selection: %v", err)
	}
	if !strings.Contains(questions["content.advert"].Instructions, ".message") || !strings.Contains(questions["profile.money"].Instructions, ".sender") || questions["target.invocation"].Type != "noul" {
		t.Fatal("image example lost the separate message and sender regions")
	}
	if !strings.Contains(questions["content.advert"].Instructions, "图片内容") || c.Model.ImageTimeoutSeconds != 60 {
		t.Fatal("image example lost visual question scope or timeout")
	}
	id := strings.TrimSpace(string(command(t, engine, "create", name, "-example")))
	cleanupContainer(t, engine, id)
	cert := filepath.Join(t.TempDir(), "ca-certificates.crt")
	command(t, engine, "cp", id+":/etc/ssl/certs/ca-certificates.crt", cert)
	pem, err := os.ReadFile(cert)
	if err != nil {
		t.Fatal(err)
	}
	if !x509.NewCertPool().AppendCertsFromPEM(pem) {
		t.Fatal("runtime has no usable HTTPS certificate roots")
	}
}

func TestContainerPersistsCommandChangesAndResumesPolling(t *testing.T) {
	testPersistentRuntime(t, false)
}

func TestComposePersistsCommandChangesAndResumesPolling(t *testing.T) {
	testPersistentRuntime(t, true)
}

func testPersistentRuntime(t *testing.T, useCompose bool) {
	t.Helper()
	engine, name := image(t)
	var phase atomic.Int32
	var batches atomic.Int32
	var punishments atomic.Int32
	var visualRequests atomic.Int32
	webp, _ := base64.StdEncoding.DecodeString("UklGRigAAABXRUJQVlA4TBsAAAAvP8FFAAdQj0oXpf8BAUnS//9kRP8z/vN/4AEA")
	resumed := make(chan int64, 8)
	tg := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
			if err := r.ParseMultipartForm(1 << 20); err != nil {
				t.Error(err)
			}
		} else {
			io.Copy(io.Discard, r.Body)
			r.Body.Close()
		}
		if r.MultipartForm != nil {
			defer r.MultipartForm.RemoveAll()
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "getMe"):
			fmt.Fprint(w, `{"ok":true,"result":{"id":123,"is_bot":true,"first_name":"Guard","username":"guard_bot"}}`)
		case strings.HasSuffix(r.URL.Path, "getWebhookInfo"):
			fmt.Fprint(w, `{"ok":true,"result":{"url":""}}`)
		case strings.HasSuffix(r.URL.Path, "getChatMember"):
			if r.FormValue("user_id") == "42" {
				fmt.Fprint(w, `{"ok":true,"result":{"status":"administrator","user":{"id":42,"first_name":"Admin"}}}`)
			} else {
				fmt.Fprint(w, `{"ok":true,"result":{"status":"member","user":{"id":50,"first_name":"User"}}}`)
			}
		case strings.HasSuffix(r.URL.Path, "getChat"):
			fmt.Fprint(w, `{"ok":true,"result":{"id":50,"type":"private","bio":"public profile"}}`)
		case strings.HasSuffix(r.URL.Path, "getFile"):
			if r.FormValue("file_id") != "preview" {
				t.Error("container selected full video instead of thumbnail")
			}
			fmt.Fprint(w, `{"ok":true,"result":{"file_id":"preview","file_unique_id":"preview-u","file_path":"stickers/preview.webp"}}`)
		case strings.HasPrefix(r.URL.Path, "/file/"):
			w.Write(webp)
		case strings.HasSuffix(r.URL.Path, "sendMessage"):
			fmt.Fprint(w, `{"ok":true,"result":{"message_id":100,"date":1,"chat":{"id":-100,"type":"supergroup"}}}`)
		case strings.HasSuffix(r.URL.Path, "getUpdates"):
			if phase.Load() == 1 {
				offset, _ := strconv.ParseInt(r.FormValue("offset"), 10, 64)
				select {
				case resumed <- offset:
				default:
				}
			}
			if batches.Add(1) == 1 {
				fmt.Fprint(w, `{"ok":true,"result":[{"update_id":501,"message":{"message_id":1,"date":1,"chat":{"id":9,"type":"private"},"from":{"id":9,"first_name":"Owner"},"text":"/as_set base debug true"}},{"update_id":502,"message":{"message_id":2,"date":1,"chat":{"id":-100,"type":"supergroup"},"from":{"id":42,"first_name":"Admin"},"text":"/as_set queue_limit 7"}},{"update_id":503,"message":{"message_id":3,"date":1,"chat":{"id":-100,"type":"supergroup"},"from":{"id":50,"first_name":"User"},"text":"sample advertisement","future_field":"retained"}},{"update_id":504,"chat_member":{"chat":{"id":-100,"type":"supergroup"},"from":{"id":42,"first_name":"Admin"},"date":1,"old_chat_member":{"status":"left","user":{"id":50,"first_name":"User"}},"new_chat_member":{"status":"member","user":{"id":50,"first_name":"User"}}}},{"update_id":505,"message":{"message_id":4,"date":1,"chat":{"id":-100,"type":"supergroup"},"from":{"id":42,"first_name":"Admin"},"new_chat_members":[{"id":50,"first_name":"User"}],"future_field":"join-retained"}},{"update_id":506,"message":{"message_id":5,"date":1,"chat":{"id":-100,"type":"supergroup"},"from":{"id":50,"first_name":"User"},"sticker":{"file_id":"full-video","file_unique_id":"video-u","is_video":true,"width":512,"height":512,"thumbnail":{"file_id":"preview","file_unique_id":"preview-u","width":320,"height":280}}}}]}`)
			} else {
				select {
				case <-r.Context().Done():
				case <-time.After(2 * time.Second):
					fmt.Fprint(w, `{"ok":true,"result":[]}`)
				}
			}
		default:
			punishments.Add(1)
			fmt.Fprint(w, `{"ok":true,"result":true}`)
		}
	}))
	t.Cleanup(tg.Close)
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Model     string                     `json:"model"`
			Questions map[string]config.Question `json:"questions"`
			State     json.RawMessage            `json:"state"`
			Assets    map[string]struct {
				Type, Format string
				Data         string `json:"data_base64"`
			} `json:"assets"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		if len(request.Assets) > 0 {
			visualRequests.Add(1)
			if r.Header.Get("TypeSafe-Danger-Zone-Images") != "true" {
				t.Error("container omitted image compatibility header")
			}
			for ref, a := range request.Assets {
				data, err := base64.StdEncoding.Strict().DecodeString(a.Data)
				if err != nil {
					t.Error(err)
				}
				cfg, format, err := stdimage.DecodeConfig(bytes.NewReader(data))
				if err != nil || a.Type != "image" || a.Format != format || cfg.Width < 280 || cfg.Height < 280 || cfg.Width > 1568 || cfg.Height > 1568 || !bytes.Contains(request.State, []byte(ref)) {
					t.Error("container sent invalid visual asset")
				}
			}
		}
		answers := map[string]any{}
		for id := range request.Questions {
			answers[id] = map[string]any{"type": "noul", "noul": 0.99}
		}
		json.NewEncoder(w).Encode(map[string]any{"model": request.Model, "answers": answers})
	}))
	t.Cleanup(model.Close)
	projectDir := t.TempDir()
	state := filepath.Join(projectDir, "runtime")
	if err := os.Mkdir(state, 0700); err != nil {
		t.Fatal(err)
	}
	c := config.Defaults()
	c.OwnerIDs = []int64{9}
	c.Model.Name = "container-test"
	c.Model.BaseURL = model.URL
	c.Telegram.APIBase = tg.URL
	join := config.DefaultJoinPolicy()
	join.Enabled = true
	c.JoinGate = &config.JoinGate{Base: join, Groups: map[string]json.RawMessage{}}
	c.QuestionGroups = map[string]map[string]config.Question{"join": {}}
	for _, id := range []string{"promotion", "money", "earnings", "sexual"} {
		c.QuestionGroups["join"][id] = config.Question{Type: "noul", Instructions: "Evaluate .member.name and .member.bio for " + id}
	}
	raw, _ := json.MarshalIndent(c, "", "  ")
	configPath := filepath.Join(state, "config.json")
	if err := os.WriteFile(configPath, raw, 0600); err != nil {
		t.Fatal(err)
	}
	user := fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid())
	containerName := fmt.Sprintf("antispam-test-%d", time.Now().UnixNano())
	var composePrefix []string
	if useCompose {
		definition, err := os.ReadFile(filepath.Join("..", "..", "compose.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		composeFile := filepath.Join(projectDir, "compose.yaml")
		if err := os.WriteFile(composeFile, definition, 0600); err != nil {
			t.Fatal(err)
		}
		envFile := filepath.Join(projectDir, ".env")
		environment := fmt.Sprintf("ANTISPAM_IMAGE=%s\nBOT_UID=%d\nBOT_GID=%d\nTELEGRAM_BOT_TOKEN=123:container-test\nSYSTEMONE_API_KEY=\n", name, os.Getuid(), os.Getgid())
		if err := os.WriteFile(envFile, []byte(environment), 0600); err != nil {
			t.Fatal(err)
		}
		composePrefix = []string{"compose", "--file", composeFile, "--project-name", containerName, "--env-file", envFile}
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			_ = containerCommand(ctx, engine, append(composePrefix, "down", "--remove-orphans")...).Run()
		})
		command(t, engine, append(composePrefix, "config", "--quiet")...)
		command(t, engine, append(composePrefix, "up", "--detach", "--no-build")...)
		containerName = strings.TrimSpace(string(command(t, engine, append(composePrefix, "ps", "--quiet")...)))
		if containerName == "" {
			t.Fatal("Compose did not create the service")
		}
	} else {
		cleanupContainer(t, engine, containerName)
		command(t, engine, "run", "--detach", "--pull=never", "--name", containerName, "--network=host", "--read-only", "--cap-drop=ALL", "--security-opt=no-new-privileges", "--user", user, "--volume", state+":/runtime:z", "--env", "TELEGRAM_BOT_TOKEN=123:container-test", name)
	}
	dbpath := filepath.Join(state, "data", "bot.sqlite")
	wait(t, func() bool {
		current, err := config.Open(configPath)
		if err != nil {
			return false
		}
		if !current.Snapshot().Base.Debug || current.Snapshot().ForGroup(-100).QueueLimit != 7 {
			return false
		}
		if _, err := os.Stat(dbpath); err != nil {
			return false
		}
		db, err := sql.Open("sqlite", dbpath+"?_pragma=busy_timeout(1000)")
		if err != nil {
			return false
		}
		defer db.Close()
		var n int
		if db.QueryRow(`SELECT count(*) FROM attempts WHERE error=''`).Scan(&n) != nil || n != 2 {
			return false
		}
		if db.QueryRow(`SELECT count(*) FROM join_actions WHERE status='recorded'`).Scan(&n) != nil || n != 1 {
			return false
		}
		return db.QueryRow(`SELECT count(*) FROM join_service_actions WHERE status='recorded'`).Scan(&n) == nil && n == 1
	}, func() string { return string(command(t, engine, "logs", containerName)) })
	// A read-only validation helper shares the same directory with the live bot.
	// A shared SELinux label keeps that helper from invalidating the live mount.
	if useCompose {
		command(t, engine, append(composePrefix, "run", "--rm", "-T", "--no-deps", "antispam", "-config", "/runtime/config.json", "-check")...)
	} else {
		command(t, engine, "run", "--rm", "--pull=never", "--network=none", "--read-only", "--user", user, "--volume", state+":/runtime:z", name, "-config", "/runtime/config.json", "-check")
	}
	command(t, engine, "stop", "--time=10", containerName)
	if exit := strings.TrimSpace(string(command(t, engine, "inspect", "--format", "{{.State.ExitCode}}", containerName))); exit != "0" {
		t.Fatalf("unclean stop: %s", exit)
	}
	phase.Store(1)
	command(t, engine, "start", containerName)
	select {
	case offset := <-resumed:
		if offset != 507 {
			t.Fatalf("lost checkpoint: offset=%d", offset)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("container did not resume polling")
	}
	command(t, engine, "stop", "--time=10", containerName)
	if punishments.Load() != 0 {
		t.Fatal("record mode issued a punishment")
	}
	db, err := sql.Open("sqlite", dbpath+"?_pragma=busy_timeout(1000)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var updates, attempts int
	if err := db.QueryRow(`SELECT count(*) FROM updates`).Scan(&updates); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM attempts`).Scan(&attempts); err != nil {
		t.Fatal(err)
	}
	if updates != 6 || attempts != 2 {
		t.Fatalf("restart lost or duplicated evidence: updates=%d attempts=%d", updates, attempts)
	}
	var joins int
	if visualRequests.Load() != 1 {
		t.Fatalf("visual requests=%d want1 across restart", visualRequests.Load())
	}
	if err := db.QueryRow(`SELECT count(*) FROM revision_images WHERE json_extract(lookups,'$[0].status')='available'`).Scan(&joins); err != nil || joins != 1 {
		t.Fatalf("container lost frozen image audit: %d %v", joins, err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM join_service_actions WHERE status='recorded'`).Scan(&joins); err != nil || joins != 1 {
		t.Fatalf("cleanup observation lost across restart: %d %v", joins, err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM join_attempts WHERE error='' AND json_extract(evaluation,'$.score')=100 AND json_extract(request,'$.state.member.bio')='public profile'`).Scan(&joins); err != nil || joins != 1 {
		t.Fatalf("join evidence lost or repeated across restart: %d %v", joins, err)
	}
	var archived string
	if err := db.QueryRow(`SELECT raw FROM revisions WHERE message_id=3`).Scan(&archived); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(archived, "retained") {
		t.Fatal("container lost original fields")
	}
	logCtx, logCancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer logCancel()
	runtimeLogs, err := containerCommand(logCtx, engine, "logs", containerName).CombinedOutput()
	if err != nil {
		t.Fatalf("read container logs: %v", err)
	}
	for _, event := range []string{`"level":"DEBUG"`, `"event":"model.answers"`, `"event":"policy.rules"`, `"event":"config.updated"`} {
		if !bytes.Contains(runtimeLogs, []byte(event)) {
			t.Fatalf("container omitted structured event %s", event)
		}
	}
	for _, private := range []string{"123:container-test", "public profile", "sample advertisement", "future_field"} {
		if bytes.Contains(runtimeLogs, []byte(private)) {
			t.Fatalf("container logged private payload %s", private)
		}
	}
}

func wait(t *testing.T, condition func() bool, diagnostic func() string) {
	t.Helper()
	deadline := time.NewTimer(20 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-deadline.C:
			t.Fatalf("container readiness timeout: %s", diagnostic())
		case <-tick.C:
			if condition() {
				return
			}
		}
	}
}
