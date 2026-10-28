package hlexchange

import (
	"encoding/json"
	"fmt"
	"os"
)

// The HTTP response or WS request ID binds this receipt to the submitted
// batch. Persist it before querying the independently lagging order projection.
type batchReceipt struct{ Raw json.RawMessage }

func (r *batchReceipt) Error() string { return "batch receipt; reconcile individual legs" }

type batchLeg struct {
	OID   uint64
	Error string
}

func parseBatchReceipt(raw json.RawMessage, count int) ([]batchLeg, error) {
	var envelope map[string]json.RawMessage
	if json.Unmarshal(raw, &envelope) != nil || len(envelope) != 2 || string(envelope["type"]) != `"order"` {
		return nil, fmt.Errorf("invalid batch receipt")
	}
	var data map[string]json.RawMessage
	var statuses []json.RawMessage
	if json.Unmarshal(envelope["data"], &data) != nil || len(data) != 1 || json.Unmarshal(data["statuses"], &statuses) != nil || count == 0 || len(statuses) != count {
		return nil, fmt.Errorf("incomplete batch receipt")
	}
	legs := make([]batchLeg, count)
	for i, raw := range statuses {
		var leg map[string]json.RawMessage
		if json.Unmarshal(raw, &leg) != nil || len(leg) != 1 {
			return nil, fmt.Errorf("ambiguous batch leg")
		}
		if e, ok := leg["error"]; ok {
			if _, terminal := finalRejection("err", e, nil); !terminal || json.Unmarshal(e, &legs[i].Error) != nil {
				return nil, fmt.Errorf("nonterminal batch leg")
			}
			continue
		}
		value, ok := leg["resting"]
		if !ok {
			value, ok = leg["filled"]
		}
		var order struct {
			OID uint64 `json:"oid"`
		}
		if !ok || json.Unmarshal(value, &order) != nil || order.OID == 0 {
			return nil, fmt.Errorf("invalid batch leg")
		}
		legs[i].OID = order.OID
	}
	return legs, nil
}

func (c *Client) saveBatchReceipt(p *pendingRequest, receipt *batchReceipt) error {
	p.BatchReceipt = append(json.RawMessage(nil), receipt.Raw...)
	return c.savePending(p)
}

// Replace the journal atomically while the caller holds the durable signer
// lock. A read deadline never discards evidence collected earlier in the pass.
func (c *Client) savePending(p *pendingRequest) error {
	f, err := os.CreateTemp(c.StateDir, ".pending-update-")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if err = json.NewEncoder(f).Encode(p); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err = os.Rename(name, c.pendingPath()); err != nil {
		return err
	}
	return syncDirectory(c.StateDir)
}
