package api

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// duplicateKeyErrors walks a syntactically valid JSON document and reports
// every object key that appears more than once within the same object.
//
// encoding/json silently keeps the last occurrence of a duplicated key. That
// last-wins behaviour would let an ambiguous request be adjudicated as if the
// earlier occurrence did not exist: two clearance switches, two device lists
// or two center frequencies for one device must reject the request rather
// than have the later value quietly decide the verdict.
//
// Field paths follow the request validation convention: the root keys
// "include_clearance" / "devices", and device fields as "devices[2].center_khz".
func duplicateKeyErrors(raw []byte) []fieldError {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var errs []fieldError
	scanJSONValue(dec, "", &errs)
	// Two duplicated containers can surface the same nested path once per
	// occurrence; keep the report stable by reporting each path only once.
	return dedupeFieldErrors(errs)
}

// scanJSONValue consumes exactly one JSON value (an object, an array or a
// scalar) from dec, reporting duplicate keys found along the way. path is the
// field path of the value being consumed.
func scanJSONValue(dec *json.Decoder, path string, errs *[]fieldError) {
	tok, err := dec.Token()
	if err != nil {
		// The document has already been decoded successfully before the
		// scanner runs; a token error here cannot be produced by valid JSON.
		return
	}
	delim, ok := tok.(json.Delim)
	if !ok {
		return // scalar value: nothing to descend into
	}
	switch delim {
	case '{':
		seen := make(map[string]bool)
		for dec.More() {
			keyTok, err := dec.Token()
			if err != nil {
				return
			}
			key := keyTok.(string)
			childPath := joinFieldPath(path, key)
			if seen[key] {
				*errs = append(*errs, fieldError{
					Field:   childPath,
					Message: fmt.Sprintf("appears more than once in the same JSON object; %q must be provided at most once", key),
				})
			} else {
				seen[key] = true
			}
			// Recurse regardless: the value of a duplicated key still has
			// to be consumed to keep the token stream aligned.
			scanJSONValue(dec, childPath, errs)
		}
		_, _ = dec.Token() // closing '}'
	case '[':
		i := 0
		for dec.More() {
			scanJSONValue(dec, fmt.Sprintf("%s[%d]", path, i), errs)
			i++
		}
		_, _ = dec.Token() // closing ']'
	}
}

// joinFieldPath appends key to parent, e.g. ("devices[0]", "id") ->
// "devices[0].id"; a root key is returned unchanged.
func joinFieldPath(parent, key string) string {
	if parent == "" {
		return key
	}
	return parent + "." + key
}

// dedupeFieldErrors collapses repeated field paths while preserving the order
// of first appearance, so the error list stays deterministic.
func dedupeFieldErrors(errs []fieldError) []fieldError {
	seen := make(map[string]bool, len(errs))
	out := make([]fieldError, 0, len(errs))
	for _, e := range errs {
		if seen[e.Field] {
			continue
		}
		seen[e.Field] = true
		out = append(out, e)
	}
	return out
}
