package cloudflarekv

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"mime/multipart"
	"net/textproto"
)

// multipartValue renders a KV write body.
//
// The single-key write endpoint takes multipart/form-data with a
// `value` part and a `metadata` part, and there is no other way to
// attach metadata to a key. The metadata is what [Cache.TTL] and
// [Cache.Exists] later read, so every write goes through here even
// when the caller asked for no expiry — a key with no marker is
// indistinguishable from one written by another program.
func multipartValue(value, metadata []byte) (body []byte, contentType string, err error) {
	if len(metadata) > MaxMetadataBytes {
		return nil, "", fmt.Errorf("drops/cache/cloudflarekv: metadata is %d bytes, limit is %d",
			len(metadata), MaxMetadataBytes)
	}
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)

	// A plain CreateFormField would set no Content-Type, and KV has
	// been known to interpret the value part as text when it is
	// absent. Declaring it octet-stream keeps a value that is not
	// valid UTF-8 intact.
	valueHeader := textproto.MIMEHeader{}
	valueHeader.Set("Content-Disposition", `form-data; name="value"`)
	valueHeader.Set("Content-Type", "application/octet-stream")
	part, err := w.CreatePart(valueHeader)
	if err != nil {
		return nil, "", err
	}
	if _, err := part.Write(value); err != nil {
		return nil, "", err
	}

	metaHeader := textproto.MIMEHeader{}
	metaHeader.Set("Content-Disposition", `form-data; name="metadata"`)
	metaHeader.Set("Content-Type", "application/json")
	metaPart, err := w.CreatePart(metaHeader)
	if err != nil {
		return nil, "", err
	}
	if _, err := metaPart.Write(metadata); err != nil {
		return nil, "", err
	}

	if err := w.Close(); err != nil {
		return nil, "", err
	}
	return buf.Bytes(), w.FormDataContentType(), nil
}

// encodeBase64 renders bytes for KV's bulk write, whose JSON body can
// carry a value only as a string. Arbitrary bytes are not valid UTF-8
// and would not survive one, so the bulk entry sets base64: true and
// this is what it carries.
func encodeBase64(v []byte) string {
	return base64.StdEncoding.EncodeToString(v)
}
