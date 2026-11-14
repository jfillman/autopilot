package triggers_test

import "encoding/json"

func jsonMarshal(v any) ([]byte, error) { return json.Marshal(v) }
