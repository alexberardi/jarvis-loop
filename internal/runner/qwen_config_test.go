package runner

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func credsFile(t *testing.T, body string, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "prod-app.env")
	if err := os.WriteFile(p, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
	os.Chmod(p, mode)
	return p
}

func qwenCfg(block string) string {
	return strings.Replace(cfgBody, `"name"`, `"qwen":`+block+`,"name"`, 1)
}

func TestQwenConfig(t *testing.T) {
	// No qwen block: no Qwen builder (qwen items build with claude).
	c, err := LoadConfig(writeCfg(t, cfgBody, 0o600))
	if err != nil || c.Qwen != nil {
		t.Fatalf("no block: %+v %v", c.Qwen, err)
	}
	creds := credsFile(t, "# prod app\nPROD_APP_ID=loop-qwen\nexport PROD_APP_KEY=\"s3cret-key\"\n", 0o600)
	c, err = LoadConfig(writeCfg(t, qwenCfg(fmt.Sprintf(`{"base_url":"http://10.0.0.107:7704/v1","credentials_file":%q}`, creds)), 0o600))
	if err != nil {
		t.Fatal(err)
	}
	q := c.Qwen
	if q.Model != "background" || q.MaxMinutes != DefaultQwenMinutes || q.OpenCode != "opencode" {
		t.Fatalf("defaults %+v", q)
	}
	// The credentials are the runner's secret: masked from fenced commands, never read by tools.
	if !slices.Contains(c.Secrets(), creds) || !slices.Contains(c.VerifyMask, creds) || !slices.Contains(c.Protected(), creds) {
		t.Fatalf("creds not protected: secrets %v mask %v", c.Secrets(), c.VerifyMask)
	}
	id, key, err := q.Credentials()
	if err != nil || id != "loop-qwen" || key != "s3cret-key" {
		t.Fatalf("credentials %q %q %v", id, key, err)
	}
	c, err = LoadConfig(writeCfg(t, qwenCfg(fmt.Sprintf(`{"base_url":"https://jarvis.lan/llm/v1","credentials_file":%q,"model":"bg","max_minutes":30,"opencode":"/opt/opencode"}`, creds)), 0o600))
	if err != nil || c.Qwen.Model != "bg" || c.Qwen.MaxMinutes != 30 || c.Qwen.OpenCode != "/opt/opencode" {
		t.Fatalf("set: %+v %v", c.Qwen, err)
	}
}

func TestQwenConfigRefusals(t *testing.T) {
	good := credsFile(t, "PROD_APP_ID=a\nPROD_APP_KEY=b\n", 0o600)
	for name, tc := range map[string]struct{ block, want string }{
		"no base_url":       {fmt.Sprintf(`{"credentials_file":%q}`, good), "base_url"},
		"bad base_url":      {fmt.Sprintf(`{"base_url":"10.0.0.107:7704","credentials_file":%q}`, good), "base_url"},
		"ftp base_url":      {fmt.Sprintf(`{"base_url":"ftp://x/v1","credentials_file":%q}`, good), "base_url"},
		"no creds":          {`{"base_url":"http://x/v1"}`, "credentials_file"},
		"missing creds":     {`{"base_url":"http://x/v1","credentials_file":"/nonexistent/prod.env"}`, "credentials_file"},
		"readable creds":    {fmt.Sprintf(`{"base_url":"http://x/v1","credentials_file":%q}`, credsFile(t, "PROD_APP_ID=a\nPROD_APP_KEY=b\n", 0o644)), "0600"},
		"no key":            {fmt.Sprintf(`{"base_url":"http://x/v1","credentials_file":%q}`, credsFile(t, "PROD_APP_ID=a\n", 0o600)), "PROD_APP_KEY"},
		"empty id":          {fmt.Sprintf(`{"base_url":"http://x/v1","credentials_file":%q}`, credsFile(t, "PROD_APP_ID=\nPROD_APP_KEY=b\n", 0o600)), "PROD_APP_ID"},
		"negative minutes":  {fmt.Sprintf(`{"base_url":"http://x/v1","credentials_file":%q,"max_minutes":-1}`, good), "max_minutes"},
		"unknown qwen key":  {fmt.Sprintf(`{"base_url":"http://x/v1","credentials_file":%q,"modle":"x"}`, good), "modle"},
		"slash in model":    {fmt.Sprintf(`{"base_url":"http://x/v1","credentials_file":%q,"model":"a/b"}`, good), "model"},
		"query in base_url": {fmt.Sprintf(`{"base_url":"http://x/v1?k=1","credentials_file":%q}`, good), "base_url"},
	} {
		_, err := LoadConfig(writeCfg(t, qwenCfg(tc.block), 0o600))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: want an error naming %q, got %v", name, tc.want, err)
		}
	}
}
