package main

import "encoding/json"

// unmarshalJSON is json.Unmarshal under a name the tests can use directly.
func unmarshalJSON(data []byte, v any) error { return json.Unmarshal(data, v) }
