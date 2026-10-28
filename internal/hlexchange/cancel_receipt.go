package hlexchange

import (
	"encoding/json"
	"fmt"
)

type cancelReceipt struct{ Raw json.RawMessage }

func (r *cancelReceipt) Error() string { return "cancel receipt; reconcile target states" }

func cancelCount(action any) int {
	switch a := action.(type) {
	case Cancel:
		return len(a.Cancels)
	case *Cancel:
		return len(a.Cancels)
	case CancelByCloid:
		return len(a.Cancels)
	case *CancelByCloid:
		return len(a.Cancels)
	}
	return 0
}

func parseCancelReceipt(raw json.RawMessage, count int) ([]string, error) {
	var envelope map[string]json.RawMessage
	var data map[string]json.RawMessage
	var statuses []json.RawMessage
	if json.Unmarshal(raw, &envelope) != nil || len(envelope) != 2 || string(envelope["type"]) != `"cancel"` || json.Unmarshal(envelope["data"], &data) != nil || len(data) != 1 || json.Unmarshal(data["statuses"], &statuses) != nil || count == 0 || len(statuses) != count {
		return nil, fmt.Errorf("incomplete cancel receipt")
	}
	errors := make([]string, count)
	for i, raw := range statuses {
		if string(raw) == `"success"` {
			continue
		}
		var leg map[string]json.RawMessage
		if json.Unmarshal(raw, &leg) != nil || len(leg) != 1 {
			return nil, fmt.Errorf("ambiguous cancel leg")
		}
		value := leg["error"]
		if _, terminal := finalRejection("err", value, nil); !terminal || json.Unmarshal(value, &errors[i]) != nil {
			return nil, fmt.Errorf("nonterminal cancel leg")
		}
	}
	return errors, nil
}

func pendingCancelCount(p *pendingRequest) (int, error) {
	var header struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(p.Action, &header) != nil || len(p.Lookups) != 0 {
		return 0, fmt.Errorf("cancel receipt/action mismatch")
	}
	switch header.Type {
	case "cancel":
		var a Cancel
		if json.Unmarshal(p.Action, &a) != nil || len(a.Cancels) != len(p.CancelOIDs) || len(p.CancelCloids) != 0 {
			break
		}
		for i, item := range a.Cancels {
			if item.OID != p.CancelOIDs[i] {
				return 0, fmt.Errorf("cancel target mismatch")
			}
		}
		return len(a.Cancels), nil
	case "cancelByCloid":
		var a CancelByCloid
		if json.Unmarshal(p.Action, &a) != nil || len(a.Cancels) != len(p.CancelCloids) || len(p.CancelOIDs) != 0 {
			break
		}
		for i, item := range a.Cancels {
			if item.Cloid.String() != p.CancelCloids[i] {
				return 0, fmt.Errorf("cancel cloid mismatch")
			}
		}
		return len(a.Cancels), nil
	}
	return 0, fmt.Errorf("cancel receipt/action mismatch")
}
