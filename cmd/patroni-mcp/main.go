package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

type Cluster struct {
	URL         string `json:"url"`
	Username    string `json:"username,omitempty"`
	PasswordEnv string `json:"password_env,omitempty"`
	CAFile      string `json:"ca_file,omitempty"`
	ClientCert  string `json:"client_cert,omitempty"`
	ClientKey   string `json:"client_key,omitempty"`
}
type Config struct {
	Clusters          map[string]Cluster `json:"clusters"`
	AllowWrites       bool               `json:"allow_writes"`
	AllowFailover     bool               `json:"allow_failover"`
	AllowConfigWrites bool               `json:"allow_config_writes"`
	TimeoutSeconds    int                `json:"timeout_seconds"`
}
type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}
type Tool struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	InputSchema any    `json:"inputSchema"`
}

var names = []string{"patroni_cluster_status", "patroni_node_status", "patroni_get_config", "patroni_replication_status", "patroni_switchover", "patroni_failover", "patroni_update_config", "patroni_show_config"}

func schema(properties map[string]any, required ...string) any {
	return map[string]any{"type": "object", "properties": properties, "required": required, "additionalProperties": false}
}
func prop(t, desc string) any { return map[string]any{"type": t, "description": desc} }
func tools() []Tool {
	c := prop("string", "Configured cluster alias")
	result := []Tool{
		{names[0], "Get Patroni cluster topology and member health", schema(map[string]any{"cluster": c}, "cluster")},
		{names[1], "Get Patroni status for a specified member URL configured as a cluster endpoint", schema(map[string]any{"cluster": c}, "cluster")},
		{names[2], "Read Patroni dynamic configuration", schema(map[string]any{"cluster": c}, "cluster")},
		{names[3], "Inspect replication members and reported lag", schema(map[string]any{"cluster": c}, "cluster")},
		{names[4], "Request planned switchover; requires allow_writes and explicit leader and candidate", schema(map[string]any{"cluster": c, "leader": prop("string", "Current leader"), "candidate": prop("string", "Target replica")}, "cluster", "leader", "candidate")},
		{names[5], "Request emergency failover; requires allow_writes and allow_failover", schema(map[string]any{"cluster": c, "candidate": prop("string", "Target replica")}, "cluster", "candidate")},
		{names[6], "Patch allowlisted Patroni dynamic configuration keys; requires allow_writes and allow_config_writes", schema(map[string]any{"cluster": c, "changes": map[string]any{"type": "object", "description": "Only loop_wait, retry_timeout, maximum_lag_on_failover, ttl", "additionalProperties": true}}, "cluster", "changes")},
		{names[7], "Show Patroni dynamic configuration as YAML, like patronictl show-config", schema(map[string]any{"cluster": c}, "cluster")},
	}
	return result
}
func response(id json.RawMessage, result any, rpcerr any) map[string]any {
	out := map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(id)}
	if rpcerr != nil {
		out["error"] = rpcerr
	} else {
		out["result"] = result
	}
	return out
}
func rpcError(code int, msg string) any { return map[string]any{"code": code, "message": msg} }
func toolError(err error) any {
	return map[string]any{"content": []any{map[string]any{"type": "text", "text": err.Error()}}, "isError": true}
}
func toolSuccess(b []byte) any {
	var pretty bytes.Buffer
	if json.Indent(&pretty, b, "", "  ") != nil {
		pretty.Write(b)
	}
	return map[string]any{"content": []any{map[string]any{"type": "text", "text": pretty.String()}}}
}
func getClient(c Cluster, timeout time.Duration) (*http.Client, error) {
	tr := &http.Transport{Proxy: nil, TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}}
	if c.CAFile != "" {
		pem, err := os.ReadFile(c.CAFile)
		if err != nil {
			return nil, err
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, errors.New("invalid CA PEM")
		}
		tr.TLSClientConfig.RootCAs = pool
	}
	if c.ClientCert != "" || c.ClientKey != "" {
		cert, err := tls.LoadX509KeyPair(c.ClientCert, c.ClientKey)
		if err != nil {
			return nil, err
		}
		tr.TLSClientConfig.Certificates = []tls.Certificate{cert}
	}
	return &http.Client{Transport: tr, Timeout: timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, nil
}
func validateConfig(cfg Config) error {
	if len(cfg.Clusters) == 0 {
		return errors.New("configure at least one cluster")
	}
	for name, c := range cfg.Clusters {
		u, err := url.Parse(c.URL)
		if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" && u.Path != "/" {
			return fmt.Errorf("cluster %s: URL must be a plain http(s) origin", name)
		}
		if c.PasswordEnv != "" && os.Getenv(c.PasswordEnv) == "" {
			return fmt.Errorf("cluster %s: password environment variable %s not set", name, c.PasswordEnv)
		}
	}
	return nil
}
func patroni(ctx context.Context, c Cluster, timeout time.Duration, method, path string, body any) ([]byte, error) {
	client, err := getClient(c, timeout)
	if err != nil {
		return nil, err
	}
	defer client.CloseIdleConnections()
	var reader io.Reader
	if body != nil {
		b, e := json.Marshal(body)
		if e != nil {
			return nil, e
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.URL, "/")+path, reader)
	if err != nil {
		return nil, err
	}
	if c.Username != "" {
		req.SetBasicAuth(c.Username, os.Getenv(c.PasswordEnv))
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	b, err := io.ReadAll(io.LimitReader(res.Body, 2<<20))
	if err != nil {
		return nil, err
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return nil, fmt.Errorf("Patroni HTTP %d: %s", res.StatusCode, string(b))
	}
	return b, nil
}
func nonempty(m map[string]json.RawMessage, key string) (string, error) {
	var s string
	if err := json.Unmarshal(m[key], &s); err != nil || strings.TrimSpace(s) == "" {
		return "", fmt.Errorf("%s must be a nonempty string", key)
	}
	return s, nil
}
func call(cfg Config, name string, raw json.RawMessage) any {
	var a map[string]json.RawMessage
	if err := json.Unmarshal(raw, &a); err != nil {
		return toolError(err)
	}
	alias, err := nonempty(a, "cluster")
	if err != nil {
		return toolError(err)
	}
	c, ok := cfg.Clusters[alias]
	if !ok {
		return toolError(errors.New("unknown cluster alias"))
	}
	timeout := time.Duration(cfg.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	method, path := "GET", "/cluster"
	var payload any
	switch name {
	case names[0], names[3]:
	case names[1]:
		path = "/patroni"
	case names[2], names[7]:
		path = "/config"
	case names[4]:
		if !cfg.AllowWrites {
			return toolError(errors.New("write operations disabled"))
		}
		leader, e := nonempty(a, "leader")
		if e != nil {
			return toolError(e)
		}
		candidate, e := nonempty(a, "candidate")
		if e != nil {
			return toolError(e)
		}
		if leader == candidate {
			return toolError(errors.New("candidate must differ from leader"))
		}
		method, path, payload = "POST", "/switchover", map[string]any{"leader": leader, "candidate": candidate}
	case names[5]:
		if !cfg.AllowWrites || !cfg.AllowFailover {
			return toolError(errors.New("failover disabled"))
		}
		candidate, e := nonempty(a, "candidate")
		if e != nil {
			return toolError(e)
		}
		method, path, payload = "POST", "/failover", map[string]any{"candidate": candidate}
	case names[6]:
		if !cfg.AllowWrites || !cfg.AllowConfigWrites {
			return toolError(errors.New("configuration writes disabled"))
		}
		var changes map[string]json.RawMessage
		if err := json.Unmarshal(a["changes"], &changes); err != nil || len(changes) == 0 {
			return toolError(errors.New("changes must be a nonempty object"))
		}
		allowed := map[string]bool{"loop_wait": true, "retry_timeout": true, "maximum_lag_on_failover": true, "ttl": true}
		for key, v := range changes {
			if !allowed[key] {
				return toolError(fmt.Errorf("configuration key %q not allowlisted", key))
			}
			var n float64
			if json.Unmarshal(v, &n) != nil || n < 0 || n != float64(int64(n)) {
				return toolError(fmt.Errorf("%s must be a nonnegative integer", key))
			}
		}
		method, path, payload = "PATCH", "/config", changes
	default:
		return toolError(errors.New("unknown tool"))
	}
	if method != "GET" {
		log.Printf("AUDIT write cluster=%q operation=%q", alias, name)
	}
	b, err := patroni(context.Background(), c, timeout, method, path, payload)
	if err != nil {
		return toolError(err)
	}
	if name == names[3] {
		var doc map[string]any
		if json.Unmarshal(b, &doc) == nil {
			b, _ = json.Marshal(map[string]any{"cluster": alias, "members": doc["members"], "scope": doc["scope"]})
		}
	}
	if name == names[7] {
		dec := json.NewDecoder(bytes.NewReader(b))
		dec.UseNumber()
		var doc any
		if err := dec.Decode(&doc); err != nil {
			return toolError(fmt.Errorf("invalid configuration JSON: %w", err))
		}
		return map[string]any{"content": []any{map[string]any{"type": "text", "text": toYAML(doc)}}}
	}
	if len(b) == 0 {
		b = []byte(`{"status":"accepted"}`)
	}
	return toolSuccess(b)
}

// toYAML renders decoded JSON in the block style of patronictl show-config
// (PyYAML safe_dump: sorted keys, list items aligned with their parent key).
func toYAML(v any) string { return strings.Join(yamlLines(v), "\n") + "\n" }
func yamlLines(v any) []string {
	switch t := v.(type) {
	case map[string]any:
		if len(t) == 0 {
			return []string{"{}"}
		}
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var out []string
		for _, k := range keys {
			child := yamlLines(t[k])
			switch c := t[k].(type) {
			case map[string]any:
				if len(c) > 0 {
					out = append(out, yamlString(k)+":")
					for _, l := range child {
						out = append(out, "  "+l)
					}
					continue
				}
			case []any:
				if len(c) > 0 {
					out = append(out, yamlString(k)+":")
					out = append(out, child...)
					continue
				}
			}
			out = append(out, yamlString(k)+": "+child[0])
		}
		return out
	case []any:
		if len(t) == 0 {
			return []string{"[]"}
		}
		var out []string
		for _, item := range t {
			child := yamlLines(item)
			out = append(out, "- "+child[0])
			for _, l := range child[1:] {
				out = append(out, "  "+l)
			}
		}
		return out
	case string:
		return []string{yamlString(t)}
	case json.Number:
		return []string{t.String()}
	case bool:
		return []string{strconv.FormatBool(t)}
	case nil:
		return []string{"null"}
	}
	return []string{fmt.Sprint(v)}
}
func yamlString(s string) string {
	if strings.ContainsAny(s, "\n\r\t\\") || strings.IndexFunc(s, func(r rune) bool { return r < 0x20 }) >= 0 {
		q, _ := json.Marshal(s)
		return string(q)
	}
	plain := s != "" && !strings.ContainsAny(s[:1], "-?:,[]{}#&*!|>'\"%@` ") && !strings.HasSuffix(s, " ") && !strings.HasSuffix(s, ":") &&
		!strings.Contains(s, ": ") && !strings.Contains(s, " #")
	switch strings.ToLower(s) {
	case "true", "false", "yes", "no", "on", "off", "y", "n", "null", "~":
		plain = false
	}
	if _, err := strconv.ParseFloat(s, 64); err == nil {
		plain = false
	}
	if plain {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}
func main() {
	log.SetOutput(os.Stderr)
	log.SetFlags(log.LstdFlags | log.LUTC)
	if len(os.Args) != 3 || os.Args[1] != "--config" {
		log.Fatal("usage: patroni-mcp --config /path/config.json")
	}
	data, err := os.ReadFile(os.Args[2])
	if err != nil {
		log.Fatal(err)
	}
	var cfg Config
	if err = json.Unmarshal(data, &cfg); err != nil {
		log.Fatal(err)
	}
	if err = validateConfig(cfg); err != nil {
		log.Fatal(err)
	}
	reader := bufio.NewReaderSize(os.Stdin, 64*1024)
	writer := bufio.NewWriter(os.Stdout)
	for {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			if err != io.EOF {
				log.Print(err)
			}
			break
		}
		if len(line) > 4<<20 {
			log.Print("oversized message ignored")
			continue
		}
		var req Request
		if json.Unmarshal(line, &req) != nil {
			continue
		}
		if len(req.ID) == 0 {
			continue
		}
		var result any
		var rpcerr any
		switch req.Method {
		case "initialize":
			result = map[string]any{"protocolVersion": "2024-11-05", "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]any{"name": "patroni-mcp", "version": "0.1.0"}}
		case "ping":
			result = map[string]any{}
		case "tools/list":
			result = map[string]any{"tools": tools()}
		case "tools/call":
			var p struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			}
			if json.Unmarshal(req.Params, &p) != nil {
				rpcerr = rpcError(-32602, "invalid tool parameters")
			} else {
				result = call(cfg, p.Name, p.Arguments)
			}
		default:
			rpcerr = rpcError(-32601, "method not found")
		}
		encoded, e := json.Marshal(response(req.ID, result, rpcerr))
		if e != nil {
			log.Print(e)
			continue
		}
		writer.Write(encoded)
		writer.WriteByte('\n')
		writer.Flush()
	}
}
