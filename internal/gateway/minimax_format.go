package gateway

import (
	"bytes"
	"io"
	"net/http"
	"regexp"
	"strconv"

	"github.com/tidwall/gjson"
	"github.com/yetone/magpie/internal/provider"
)

var miniMaxTypeIndex = regexp.MustCompile(`(?s)^invalid params, Mismatch type string with value array .*?at index ([0-9]+):.*\(2013\)$`)

// MiniMax names a byte offset, not response_format, when its output-schema
// parser rejects a type array. Identify that field for the existing format
// fallback only when the offset in the final wire body proves where it failed.
func miniMaxFormatError(p provider.Provider, to provider.Protocol, sent []byte, res *http.Response) *http.Response {
	if res == nil || res.Body == nil || res.StatusCode != 400 || to != provider.Chat || res.Header.Get("Content-Encoding") != "" {
		return res
	}
	switch provider.HostOf(p.Base(to)) {
	case "api.minimaxi.com", "api.minimax.io":
	default:
		return res
	}
	if gjson.GetBytes(sent, "response_format.type").String() != "json_schema" {
		return res
	}
	schema := gjson.GetBytes(sent, "response_format.json_schema.schema")
	if !schema.IsObject() || schema.Index <= 0 {
		return res
	}
	original := res.Body
	b, err := io.ReadAll(io.LimitReader(original, (64<<10)+1))
	res.Body = readCloser{io.MultiReader(bytes.NewReader(b), original), original}
	if err != nil || len(b) > 64<<10 || !gjson.ValidBytes(b) {
		return res
	}
	e := gjson.GetBytes(b, "error")
	match := miniMaxTypeIndex.FindStringSubmatch(e.Get("message").String())
	if !e.IsObject() || e.Get("param").Exists() || len(match) != 2 {
		return res
	}
	at, err := strconv.Atoi(match[1])
	if err != nil || at < schema.Index || at >= schema.Index+len(schema.Raw) {
		return res
	}
	var typeArray bool
	walkSchema(schema, func(s gjson.Result) {
		if typ := s.Get("type"); typ.IsArray() && typ.Index == at {
			typeArray = true
		}
	})
	if !typeArray {
		return res
	}
	// Preserve the vendor's message and other fields; param is inferred by magpie.
	nb := append([]byte(nil), b[:e.Index+1]...)
	nb = append(nb, `"param":"response_format",`...)
	nb = append(nb, b[e.Index+1:]...)
	original.Close()
	res.Body = io.NopCloser(bytes.NewReader(nb))
	res.ContentLength = int64(len(nb))
	if res.Header == nil {
		res.Header = make(http.Header)
	}
	res.Header.Set("Content-Length", strconv.Itoa(len(nb)))
	return res
}
