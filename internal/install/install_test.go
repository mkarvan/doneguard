package install

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const exe = "/opt/doneguard/bin/doneguard"

const existingClaude = `{
  "model": "opus",
  "permissions": {"allow": ["Bash(go test:*)"]},
  "hooks": {
    "Stop": [
      {"matcher": "", "hooks": [{"type": "command", "command": "say done", "timeout": 5}]}
    ],
    "PreToolUse": [
      {"matcher": "Bash", "hooks": [{"type": "command", "command": "./guard.sh"}]}
    ]
  },
  "zzz_last": true
}
`

func readJSON(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("%s: %v\n%s", path, err, data)
	}
	return m
}

func commands(t *testing.T, root map[string]any, event string) []string {
	t.Helper()
	hooks, _ := root["hooks"].(map[string]any)
	groups, _ := hooks[event].([]any)
	var out []string
	for _, g := range groups {
		for _, h := range g.(map[string]any)["hooks"].([]any) {
			out = append(out, h.(map[string]any)["command"].(string))
		}
	}
	return out
}

func countDG(cmds []string) int {
	n := 0
	for _, c := range cmds {
		if strings.Contains(c, "doneguard hook") {
			n++
		}
	}
	return n
}

func TestInstallMergesWithoutClobberingAndIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	claudePath := filepath.Join(dir, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(claudePath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(claudePath, []byte(existingClaude), 0o600); err != nil {
		t.Fatal(err)
	}
	opts := Options{ProjectDir: dir, Exe: exe}
	res, err := Install(opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 2 || res[0].Action != "updated" || res[1].Action != "created" {
		t.Fatalf("results = %+v", res)
	}
	first, _ := os.ReadFile(claudePath)

	root := readJSON(t, claudePath)
	if root["model"] != "opus" || root["zzz_last"] != true || root["permissions"] == nil {
		t.Errorf("other settings lost: %v", root)
	}
	stop := commands(t, root, "Stop")
	if len(stop) != 2 || stop[0] != "say done" || stop[1] != exe+" hook --source claude" {
		t.Errorf("Stop = %q", stop)
	}
	if sub := commands(t, root, "SubagentStop"); len(sub) != 1 || countDG(sub) != 1 {
		t.Errorf("SubagentStop = %q", sub)
	}
	if pre := commands(t, root, "PreToolUse"); len(pre) != 1 || pre[0] != "./guard.sh" {
		t.Errorf("PreToolUse = %q", pre)
	}
	// Key order preserved (model first, zzz_last last).
	s := string(first)
	if !(strings.Index(s, `"model"`) < strings.Index(s, `"permissions"`) && strings.Index(s, `"permissions"`) < strings.Index(s, `"hooks"`) && strings.Index(s, `"hooks"`) < strings.Index(s, `"zzz_last"`)) {
		t.Errorf("key order changed:\n%s", s)
	}
	if st, _ := os.Stat(claudePath); st.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v", st.Mode().Perm())
	}
	// Timeout + status message.
	hooks := root["hooks"].(map[string]any)
	entry := hooks["Stop"].([]any)[1].(map[string]any)["hooks"].([]any)[0].(map[string]any)
	if entry["timeout"].(float64) != DefaultTimeout || entry["statusMessage"] != StatusMessage || entry["type"] != "command" {
		t.Errorf("entry = %v", entry)
	}

	// Codex file created with description and both events.
	codexPath := filepath.Join(dir, ".codex", "hooks.json")
	codex := readJSON(t, codexPath)
	if codex["description"] == nil {
		t.Error("new codex hooks.json should have a description")
	}
	if c := commands(t, codex, "Stop"); len(c) != 1 || c[0] != exe+" hook --source codex" {
		t.Errorf("codex Stop = %q", c)
	}
	if c := commands(t, codex, "SubagentStop"); countDG(c) != 1 {
		t.Errorf("codex SubagentStop = %q", c)
	}

	// Second install: no duplicates, files byte-identical.
	res, err = Install(opts)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range res {
		if r.Action != "unchanged" {
			t.Errorf("second install: %+v", r)
		}
	}
	second, _ := os.ReadFile(claudePath)
	if string(first) != string(second) {
		t.Errorf("second install changed file:\n%s\n---\n%s", first, second)
	}
	if n := countDG(commands(t, readJSON(t, claudePath), "Stop")); n != 1 {
		t.Errorf("duplicate DoneGuard entries: %d", n)
	}
}

func TestInstallUpdatesPathInPlace(t *testing.T) {
	dir := t.TempDir()
	if _, err := Install(Options{ProjectDir: dir, Exe: "/old/doneguard", Agents: []string{"claude"}}); err != nil {
		t.Fatal(err)
	}
	res, err := Install(Options{ProjectDir: dir, Exe: "/new path/doneguard", Agents: []string{"claude"}, Timeout: 900})
	if err != nil {
		t.Fatal(err)
	}
	if res[0].Action != "updated" {
		t.Fatalf("res = %+v", res)
	}
	root := readJSON(t, filepath.Join(dir, ".claude", "settings.json"))
	stop := commands(t, root, "Stop")
	if len(stop) != 1 || stop[0] != "'/new path/doneguard' hook --source claude" {
		t.Errorf("Stop = %q", stop)
	}
	if _, err := os.Stat(filepath.Join(dir, ".codex")); err == nil {
		t.Error("codex should not be touched with Agents=[claude]")
	}
}

func TestUninstallLeavesOthers(t *testing.T) {
	dir := t.TempDir()
	claudePath := filepath.Join(dir, ".claude", "settings.json")
	os.MkdirAll(filepath.Dir(claudePath), 0o755)
	os.WriteFile(claudePath, []byte(existingClaude), 0o644)
	if _, err := Install(Options{ProjectDir: dir, Exe: exe}); err != nil {
		t.Fatal(err)
	}
	res, err := Uninstall(Options{ProjectDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if res[0].Action != "removed" || !strings.Contains(res[0].Detail, "removed 2") {
		t.Errorf("res = %+v", res)
	}
	root := readJSON(t, claudePath)
	if stop := commands(t, root, "Stop"); len(stop) != 1 || stop[0] != "say done" {
		t.Errorf("Stop = %q", stop)
	}
	hooks := root["hooks"].(map[string]any)
	if _, ok := hooks["SubagentStop"]; ok {
		t.Error("empty SubagentStop should be removed")
	}
	if root["model"] != "opus" || hooks["PreToolUse"] == nil {
		t.Errorf("lost settings: %v", root)
	}
	// Codex file created by us becomes a minimal valid file, not deleted.
	codex := readJSON(t, filepath.Join(dir, ".codex", "hooks.json"))
	if _, ok := codex["hooks"]; ok {
		t.Errorf("codex hooks should be empty: %v", codex)
	}
	// Second uninstall is a no-op.
	res, _ = Uninstall(Options{ProjectDir: dir})
	for _, r := range res {
		if r.Action != "unchanged" {
			t.Errorf("second uninstall: %+v", r)
		}
	}
}

func TestUninstallSharedGroupKeepsOtherHooks(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, ".claude", "settings.json")
	os.MkdirAll(filepath.Dir(p), 0o755)
	os.WriteFile(p, []byte(`{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"/usr/bin/doneguard hook"},{"type":"command","command":"notify-send done"}]}]}}`), 0o644)
	if _, err := Uninstall(Options{ProjectDir: dir, Agents: []string{"claude"}}); err != nil {
		t.Fatal(err)
	}
	if stop := commands(t, readJSON(t, p), "Stop"); len(stop) != 1 || stop[0] != "notify-send done" {
		t.Errorf("Stop = %q", stop)
	}
}

func TestInvalidJSONLeftUntouched(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, ".claude", "settings.json")
	os.MkdirAll(filepath.Dir(p), 0o755)
	bad := "{ \"hooks\": [ oops"
	os.WriteFile(p, []byte(bad), 0o644)
	if _, err := Install(Options{ProjectDir: dir, Exe: exe, Agents: []string{"claude"}}); err == nil {
		t.Fatal("expected error")
	}
	data, _ := os.ReadFile(p)
	if string(data) != bad {
		t.Error("invalid file was modified")
	}
	os.WriteFile(p, []byte(`{"hooks": "nope"}`), 0o644)
	if _, err := Install(Options{ProjectDir: dir, Exe: exe, Agents: []string{"claude"}}); err == nil {
		t.Fatal("expected error for non-object hooks")
	}
}

func TestGlobalAndLocalTargets(t *testing.T) {
	home := t.TempDir()
	ts, err := Targets(Options{Global: true, Home: home})
	if err != nil {
		t.Fatal(err)
	}
	if ts[0].Path != filepath.Join(home, ".claude", "settings.json") || ts[1].Path != filepath.Join(home, ".codex", "hooks.json") {
		t.Errorf("targets = %+v", ts)
	}
	ts, _ = Targets(Options{ProjectDir: "/p", ClaudeLocal: true, Agents: []string{"claude"}})
	if len(ts) != 1 || ts[0].Path != filepath.Join("/p", ".claude", "settings.local.json") {
		t.Errorf("targets = %+v", ts)
	}
	if _, err := Targets(Options{Agents: []string{"cursor"}}); err == nil {
		t.Error("unknown agent should error")
	}
}

func TestIsDoneGuardHook(t *testing.T) {
	mk := func(kv ...string) *Object {
		o := NewObject()
		for i := 0; i < len(kv); i += 2 {
			o.Set(kv[i], kv[i+1])
		}
		return o
	}
	yes := []*Object{
		mk("command", "doneguard hook"),
		mk("command", "/usr/local/bin/doneguard hook --source codex"),
		mk("command", `"C:\Program Files\doneguard.exe" hook`),
		mk("command", "'/a b/doneguard' hook"),
		mk("command", "/opt/doneguard hook --source=claude --exit2"),
	}
	for _, o := range yes {
		if !IsDoneGuardHook(o) {
			t.Errorf("should match: %v", o.Vals)
		}
	}
	no := []any{mk("command", "doneguard-hooker"), mk("command", "echo doneguard hookup"), mk("command", "doneguard report"), "string", nil,
		// A display label alone does not make an entry ours.
		mk("command", "something else", "statusMessage", StatusMessage),
		// Custom scripts whose name merely ends in doneguard.
		mk("command", "/usr/local/bin/my-doneguard hook"),
		mk("command", "./scripts/notdoneguard hook --source claude"),
		// Compound commands that invoke DoneGuard alongside something else.
		mk("command", "doneguard hook && notify-send done"),
		mk("command", "doneguard hook; ./cleanup.sh"),
		mk("command", "doneguard hook | tee log"),
		mk("command", "DONEGUARD_DISABLE=$X doneguard hook"),
		mk("command", "doneguard hook --source claude --extra"),
		mk("command", "sh -c 'doneguard hook'"),
	}
	for _, o := range no {
		if IsDoneGuardHook(o) {
			t.Errorf("should not match: %v", o)
		}
	}
}

func TestInstallLeavesScopedGroupAndUsesUnrestrictedGroup(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, ".claude", "settings.json")
	os.MkdirAll(filepath.Dir(p), 0o755)
	// A legacy DoneGuard entry inside a group scoped to one subagent type.
	os.WriteFile(p, []byte(`{"hooks":{"SubagentStop":[{"matcher":"Explore","hooks":[{"type":"command","command":"doneguard hook"},{"type":"command","command":"./explore-check.sh"}]}]}}`), 0o644)
	if _, err := Install(Options{ProjectDir: dir, Exe: exe, Agents: []string{"claude"}}); err != nil {
		t.Fatal(err)
	}
	root := readJSON(t, p)
	groups := root["hooks"].(map[string]any)["SubagentStop"].([]any)
	if len(groups) != 2 {
		t.Fatalf("groups = %v", groups)
	}
	scoped := groups[0].(map[string]any)
	if scoped["matcher"] != "Explore" || len(scoped["hooks"].([]any)) != 1 {
		t.Errorf("scoped group should keep only its own hook: %v", scoped)
	}
	managed := groups[1].(map[string]any)
	if _, ok := managed["matcher"]; ok {
		t.Errorf("managed hook must be in an unrestricted group: %v", managed)
	}
}

func TestInstallRefusesSymlinkEscape(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on windows")
	}
	dir := t.TempDir()
	outside := t.TempDir()
	userSettings := filepath.Join(outside, "settings.json")
	os.WriteFile(userSettings, []byte(`{"model":"mine"}`), 0o644)
	// Leaf symlink and parent-directory symlink.
	os.MkdirAll(filepath.Join(dir, ".claude"), 0o755)
	os.Symlink(userSettings, filepath.Join(dir, ".claude", "settings.json"))
	os.Symlink(outside, filepath.Join(dir, ".codex"))
	if _, err := Install(Options{ProjectDir: dir, Exe: exe, Agents: []string{"claude"}}); err == nil || !strings.Contains(err.Error(), "outside") {
		t.Fatalf("leaf symlink escape: err = %v", err)
	}
	if _, err := Install(Options{ProjectDir: dir, Exe: exe, Agents: []string{"codex"}}); err == nil {
		t.Fatal("parent symlink escape should fail")
	}
	if _, err := Uninstall(Options{ProjectDir: dir, Agents: []string{"claude"}}); err == nil {
		t.Fatal("uninstall through an escaping symlink should fail")
	}
	if data, _ := os.ReadFile(userSettings); string(data) != `{"model":"mine"}` {
		t.Errorf("file outside the project was modified: %s", data)
	}
	if _, err := os.Stat(filepath.Join(outside, "hooks.json")); err == nil {
		t.Error("wrote hooks.json outside the project")
	}
}

func TestOrderedJSONRoundTrip(t *testing.T) {
	in := `{"b":1,"a":[true,null,"x<y>&z",1.50,{}],"c":{"z":[],"y":"\u00e9"}}`
	v, err := Parse([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	out := string(Marshal(v))
	if !strings.HasPrefix(out, "{\n  \"b\": 1,\n  \"a\": [") || !strings.Contains(out, `"x<y>&z"`) || !strings.Contains(out, "1.50") || !strings.Contains(out, `"y": "é"`) {
		t.Errorf("out = %s", out)
	}
	var check any
	if err := json.Unmarshal([]byte(out), &check); err != nil {
		t.Fatal(err)
	}
	if _, err := Parse([]byte(`{} {}`)); err == nil {
		t.Error("trailing data should error")
	}
}

func TestRenamedExeIsManagedAcrossReinstallAndUninstall(t *testing.T) {
	dir := t.TempDir()
	renamed := "/opt/tools/dg-renamed"
	opts := Options{ProjectDir: dir, Exe: renamed, Agents: []string{"claude"}}
	for i := 0; i < 2; i++ {
		if _, err := Install(opts); err != nil {
			t.Fatal(err)
		}
	}
	p := filepath.Join(dir, ".claude", "settings.json")
	for _, ev := range Events {
		if cmds := commands(t, readJSON(t, p), ev); len(cmds) != 1 || cmds[0] != HookCommand(renamed, "claude") {
			t.Fatalf("%s: re-install duplicated or lost the hook: %q", ev, cmds)
		}
	}
	if _, err := Uninstall(Options{ProjectDir: dir, Exe: renamed, Agents: []string{"claude"}}); err != nil {
		t.Fatal(err)
	}
	for _, ev := range Events {
		if cmds := commands(t, readJSON(t, p), ev); len(cmds) != 0 {
			t.Fatalf("%s: uninstall left the hook behind: %q", ev, cmds)
		}
	}
	// Another program with the same argument shape is not claimed.
	if isManagedCommand("/opt/tools/other hook --source claude", renamed) {
		t.Error("claimed a hook for a different executable")
	}
}
