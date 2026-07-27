package viewbook

import "fmt"

// changes says what someone did, in words, rather than handing over a diff of
// the whole file. What reaches the session has to read as a request, because
// that is what it is.
func changes(before, after map[string]any) []string {
	var said []string
	for _, kind := range []string{"views", "requirements", "states", "stories"} {
		was := byUID(before[kind])
		now := byUID(after[kind])
		for uid, item := range now {
			old, existed := was[uid]
			if !existed {
				said = append(said, fmt.Sprintf("added %s %s", singular(kind), title(item, uid)))
				continue
			}
			if note, old := text(item, "notes"), text(old, "notes"); note != old {
				if note == "" {
					said = append(said, fmt.Sprintf("cleared the note on %s", title(item, uid)))
				} else {
					said = append(said, fmt.Sprintf("note on %s: %s", title(item, uid), note))
				}
			}
			if now, was := text(item, "status"), text(old, "status"); now != was {
				said = append(said, fmt.Sprintf("%s: %s is now %s", title(item, uid), was, now))
			}
			if text(item, "statement") != text(old, "statement") {
				said = append(said, fmt.Sprintf("reworded %s: %s", title(item, uid), text(item, "statement")))
			}
		}
		for uid, item := range was {
			if _, kept := now[uid]; !kept {
				said = append(said, fmt.Sprintf("removed %s %s", singular(kind), title(item, uid)))
			}
		}
	}
	return said
}

func byUID(list any) map[string]map[string]any {
	out := map[string]map[string]any{}
	items, ok := list.([]any)
	if !ok {
		return out
	}
	for _, entry := range items {
		item, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		if uid, ok := item["uid"].(string); ok {
			out[uid] = item
		}
	}
	return out
}

func text(item map[string]any, field string) string {
	if item == nil {
		return ""
	}
	value, _ := item[field].(string)
	return value
}

func title(item map[string]any, uid string) string {
	if name := text(item, "title"); name != "" {
		return name
	}
	return uid
}

func singular(kind string) string {
	return kind[:len(kind)-1]
}
