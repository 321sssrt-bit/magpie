package gateway

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/tidwall/gjson"
	"github.com/yetone/magpie/internal/provider"
)

const miniMaxMemorySchema = `{"type":"object","properties":{"rollout_summary":{"type":"string"},"rollout_slug":{"type":["string","null"]},"raw_memory":{"type":"string"}},"required":["rollout_summary","rollout_slug","raw_memory"],"additionalProperties":false}`

func miniMaxParserError(t *testing.T, index int) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/minimax_format_error.json")
	if err != nil {
		t.Fatal(err)
	}
	// The captured vendor error's byte offset follows the request as sent.
	return []byte(strings.Replace(string(b), "at index 952:", "at index "+strconv.Itoa(index)+":", 1))
}

func TestMiniMaxMemoryFormatFallback(t *testing.T) {
	for _, schema := range []struct{ name, body string }{
		{"plain", miniMaxMemorySchema},
		{"escaped", strings.Replace(miniMaxMemorySchema, `"type":[`, `"ty\u0070e":[`, 1)},
	} {
		for _, host := range []string{"api.minimaxi.com", "api.minimax.io"} {
			for _, tc := range []struct{ path, body string }{
				{"/v1/responses", `{"model":"mm/MiniMax-M3.1-Flash","input":"Return JSON with rollout_slug null.","text":{"format":{"type":"json_schema","name":"memory","strict":true,"schema":` + schema.body + `}}}`},
				{"/v1/chat/completions", `{"model":"mm/MiniMax-M3.1-Flash","messages":[{"role":"user","content":"Return JSON with rollout_slug null."}],"response_format":{"type":"json_schema","json_schema":{"name":"memory","strict":true,"schema":` + schema.body + `}}}`},
				{"/v1/messages", `{"model":"mm/MiniMax-M3.1-Flash","max_tokens":128,"messages":[{"role":"user","content":"Return JSON with rollout_slug null."}],"output_config":{"format":{"type":"json_schema","schema":` + schema.body + `}}}`},
			} {
				t.Run(schema.name+"/"+host+tc.path, func(t *testing.T) {
					fresh(t)
					p := provider.Provider{ID: "mm", Name: "MiniMax", Key: "test", Chat: "https://" + host + "/v1", Models: []string{"MiniMax-M3.1-Flash"}}
					if schema.name == "plain" {
						p.ExtraBody = provider.ExtraBody{"a_padding": json.RawMessage(`"the offset must include provider fields"`)}
					}
					if err := provider.Save(p); err != nil {
						t.Fatal(err)
					}
					var mu sync.Mutex
					var sent [][]byte
					s := New()
					s.client = &http.Client{Transport: countTransport(func(r *http.Request) (*http.Response, error) {
						b, err := io.ReadAll(r.Body)
						if err != nil {
							t.Fatal(err)
						}
						mu.Lock()
						sent = append(sent, b)
						mu.Unlock()
						if r.URL.Host != host || r.URL.Path != "/v1/chat/completions" {
							t.Fatalf("unexpected destination %s", r.URL)
						}
						if gjson.GetBytes(b, "response_format.type").String() == "json_schema" {
							if schema.name == "escaped" && !bytes.Contains(b, []byte(`"ty\u0070e"`)) {
								t.Fatal("fixture lost its escaped key on the wire")
							}
							at := gjson.GetBytes(b, "response_format.json_schema.schema.properties.rollout_slug.type")
							if !at.IsArray() {
								t.Fatalf("schema was narrowed before asking vendor: %s", b)
							}
							return &http.Response{StatusCode: 400, Header: http.Header{"Content-Type": {"application/json"}},
								Body: io.NopCloser(strings.NewReader(string(miniMaxParserError(t, at.Index))))}, nil
						}
						if gjson.GetBytes(b, "response_format.type").String() != "json_object" {
							t.Fatalf("want existing json_object fallback: %s", b)
						}
						if !strings.Contains(gjson.GetBytes(b, "messages.0.content").String(), schema.body) {
							t.Errorf("original schema, including null, missing from instruction: %s", b)
						}
						text, _ := json.Marshal(`{"rollout_summary":"ok","rollout_slug":null,"raw_memory":"ok"}`)
						reply := `{"id":"c","object":"chat.completion","model":"MiniMax-M3.1-Flash","choices":[{"index":0,"message":{"role":"assistant","content":` + string(text) + `},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`
						contentType := "application/json"
						if gjson.GetBytes(b, "stream").Bool() {
							reply = "data: " + strings.ReplaceAll(strings.ReplaceAll(reply, `"message":`, `"delta":`), `"chat.completion"`, `"chat.completion.chunk"`) + "\n\ndata: [DONE]\n\n"
							contentType = "text/event-stream"
						}
						return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {contentType}},
							Body: io.NopCloser(strings.NewReader(reply))}, nil
					})}
					for turn := 0; turn < 2; turn++ {
						rec := httptest.NewRecorder()
						s.Handler().ServeHTTP(rec, httptest.NewRequest("POST", tc.path, strings.NewReader(tc.body)))
						if rec.Code != 200 || !strings.Contains(rec.Body.String(), `rollout_slug`) {
							t.Errorf("turn %d: %d %s", turn, rec.Code, rec.Body.String())
						}
						mu.Lock()
						n := len(sent)
						mu.Unlock()
						if want := 2 + turn; n != want {
							t.Errorf("turn %d: sent %d requests, want %d", turn, n, want)
						}
					}
				})
			}
		}
	}
}

func TestMiniMaxFormatErrorOnlyChat(t *testing.T) {
	sent := []byte(`{"response_format":{"type":"json_schema","json_schema":{"schema":` + miniMaxMemorySchema + `}}}`)
	at := gjson.GetBytes(sent, "response_format.json_schema.schema.properties.rollout_slug.type").Index
	newReply := func() *http.Response {
		b := miniMaxParserError(t, at)
		return &http.Response{StatusCode: 400, Header: http.Header{}, Body: io.NopCloser(bytes.NewReader(b))}
	}
	for _, tc := range []struct {
		to   provider.Protocol
		base string
		want bool
	}{
		{provider.Chat, "https://api.minimaxi.com/v1", true},
		{provider.Anthropic, "https://api.minimaxi.com/anthropic", false},
	} {
		t.Run(string(tc.to), func(t *testing.T) {
			p := provider.Provider{Chat: tc.base, Anthropic: tc.base}
			res := miniMaxFormatError(p, tc.to, sent, newReply())
			b, err := io.ReadAll(res.Body)
			res.Body.Close()
			if err != nil {
				t.Fatal(err)
			}
			if got := gjson.GetBytes(b, "error.param").Exists(); got != tc.want {
				t.Errorf("param present = %v, want %v: %s", got, tc.want, b)
			}
		})
	}
}

func TestMiniMaxFormatErrorBoundaries(t *testing.T) {
	sent := []byte(`{"messages":[{"role":"user","content":"` + "\u6d4b\u8bd5" + `"}],"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":["object","null"]}}}],"response_format":{"type":"json_schema","json_schema":{"name":"memory","schema":` + miniMaxMemorySchema + `}}}`)
	at := gjson.GetBytes(sent, "response_format.json_schema.schema.properties.rollout_slug.type").Index
	scalarAt := gjson.GetBytes(sent, "response_format.json_schema.schema.properties.rollout_summary.type").Index
	toolAt := gjson.GetBytes(sent, "tools.0.function.parameters.type").Index
	valid := miniMaxParserError(t, at)
	if match := miniMaxTypeIndex.FindStringSubmatch(gjson.GetBytes(valid, "error.message").String()); len(match) != 2 {
		t.Fatal("captured MiniMax error no longer matches the classifier")
	}
	padError := func(size int) []byte {
		return append(bytes.Clone(valid), bytes.Repeat([]byte(" "), size-len(valid))...)
	}
	p := provider.Provider{Chat: "https://api.minimaxi.com/v1"}
	for _, tc := range []struct {
		name, host, encoding string
		to                   provider.Protocol
		status               int
		request, response    []byte
		annotated            bool
	}{
		{"matching byte offset", p.Chat, "", provider.Chat, 400, sent, valid, true},
		{"different host", "https://other.test/v1", "", provider.Chat, 400, sent, valid, false},
		{"lookalike host", "https://api.minimaxi.com.evil.test/v1", "", provider.Chat, 400, sent, valid, false},
		{"different protocol", p.Chat, "", provider.Anthropic, 400, sent, valid, false},
		{"different status", p.Chat, "", provider.Chat, 429, sent, valid, false},
		{"encoded response", p.Chat, "gzip", provider.Chat, 400, sent, valid, false},
		{"tool offset", p.Chat, "", provider.Chat, 400, sent, miniMaxParserError(t, toolAt), false},
		{"offset outside request", p.Chat, "", provider.Chat, 400, sent, miniMaxParserError(t, len(sent)+1), false},
		{"scalar type value", p.Chat, "", provider.Chat, 400, sent, miniMaxParserError(t, scalarAt), false},
		{"not type value", p.Chat, "", provider.Chat, 400, sent, miniMaxParserError(t, at+1), false},
		{"different error code", p.Chat, "", provider.Chat, 400, sent, bytes.Replace(valid, []byte("(2013)"), []byte("(2014)"), 1), false},
		{"existing vendor param", p.Chat, "", provider.Chat, 400, sent, bytes.Replace(valid, []byte(`"error": {`), []byte(`"error": {"param":"tools",`), 1), false},
		{"no output schema", p.Chat, "", provider.Chat, 400, []byte(`{"tools":[]}`), valid, false},
		{"invalid error JSON", p.Chat, "", provider.Chat, 400, sent, []byte(`{"error":`), false},
		{"error below limit", p.Chat, "", provider.Chat, 400, sent, padError((64 << 10) - 1), true},
		{"error at limit", p.Chat, "", provider.Chat, 400, sent, padError(64 << 10), true},
		{"error above limit", p.Chat, "", provider.Chat, 400, sent, padError((64 << 10) + 1), false},
		{"oversized error preserved", p.Chat, "", provider.Chat, 400, sent, append(bytes.Clone(valid), bytes.Repeat([]byte(" "), 70<<10)...), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := provider.Provider{Chat: tc.host}
			res := &http.Response{StatusCode: tc.status, Body: io.NopCloser(bytes.NewReader(tc.response)),
				Header: http.Header{"Content-Encoding": {tc.encoding}}, ContentLength: int64(len(tc.response))}
			before := bytes.Clone(tc.request)
			got := miniMaxFormatError(p, tc.to, tc.request, res)
			b, err := io.ReadAll(got.Body)
			got.Body.Close()
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, tc.request) {
				t.Fatal("request mutated")
			}
			if !tc.annotated {
				if !bytes.Equal(b, tc.response) || got.ContentLength != int64(len(tc.response)) {
					t.Fatal("unrelated error changed or truncated")
				}
				return
			}
			if gjson.GetBytes(b, "error.param").String() != "response_format" {
				t.Fatalf("output schema error not identified: %s", b)
			}
			if gjson.GetBytes(b, "error.message").String() != gjson.GetBytes(tc.response, "error.message").String() {
				t.Fatal("vendor error message changed")
			}
			if got.ContentLength != int64(len(b)) || got.Header.Get("Content-Length") != strconv.Itoa(len(b)) {
				t.Fatal("wrong annotated response length")
			}
		})
	}
}

func TestMiniMaxFormatErrorSchemaKeys(t *testing.T) {
	for _, tc := range []struct {
		name, schema string
		annotated    bool
	}{
		{"nested schema", `{"$defs":{"item":{"anyOf":[{"type":["string","null"]}]}}}`, true},
		{"escaped type", `{"properties":{"x":{"ty\u0070e":["string","null"]}}}`, true},
		{"different key ending in type", `{"type":"object","x-\"type":["string","null"]}`, false},
		{"constant data", `{"type":"object","const":{"type":["string","null"]}}`, false},
		{"example data", `{"type":"object","examples":[{"type":["string","null"]}]}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sent := []byte(`{"response_format":{"type":"json_schema","json_schema":{"schema":` + tc.schema + `}}}`)
			at := bytes.Index(sent, []byte(`["string","null"]`))
			if !json.Valid(sent) || at < 0 {
				t.Fatal("invalid schema fixture")
			}
			original := miniMaxParserError(t, at)
			res := &http.Response{StatusCode: 400, Body: io.NopCloser(bytes.NewReader(original))}
			p := provider.Provider{Chat: "https://api.minimaxi.com/v1"}
			res = miniMaxFormatError(p, provider.Chat, sent, res)
			b, err := io.ReadAll(res.Body)
			res.Body.Close()
			if err != nil {
				t.Fatal(err)
			}
			if got := gjson.GetBytes(b, "error.param").String() == "response_format"; got != tc.annotated {
				t.Fatalf("annotated = %v, want %v: %s", got, tc.annotated, b)
			}
			if !tc.annotated && !bytes.Equal(b, original) {
				t.Fatal("unrelated error changed")
			}
		})
	}
}
