package console

import (
	"bytes"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
)

// Diff turns two JSON documents into the RFC 6902 operations (add, remove,
// replace) that make prev into next. Objects diff by key. An array whose
// length changed is replaced whole; one of the same length diffs element by
// element.
func Diff(prev, next []byte) ([]PatchOp, error) {
	a, err := decode(prev)
	if err != nil {
		return nil, err
	}
	b, err := decode(next)
	if err != nil {
		return nil, err
	}
	ops := []PatchOp{}
	err = diffValue("", a, b, &ops)
	return ops, err
}

func decode(b []byte) (any, error) {
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber() // ids and byte counts must not round through float64
	var v any
	if err := d.Decode(&v); err != nil {
		return nil, err
	}
	return v, nil
}

func diffValue(path string, a, b any, ops *[]PatchOp) error {
	switch av := a.(type) {
	case map[string]any:
		if bv, ok := b.(map[string]any); ok {
			return diffObject(path, av, bv, ops)
		}
	case []any:
		if bv, ok := b.([]any); ok && len(av) == len(bv) {
			for i := range av {
				if err := diffValue(path+"/"+strconv.Itoa(i), av[i], bv[i], ops); err != nil {
					return err
				}
			}
			return nil
		}
	default:
		if a == b { // json.Number, string, bool and nil compare by value
			return nil
		}
	}
	return emit(ops, "replace", path, b)
}

func diffObject(path string, a, b map[string]any, ops *[]PatchOp) error {
	keys := make([]string, 0, len(a)+len(b))
	for k := range a {
		keys = append(keys, k)
	}
	for k := range b {
		if _, ok := a[k]; !ok {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		p := path + "/" + escape(k)
		av, inA := a[k]
		bv, inB := b[k]
		switch {
		case !inB:
			if err := emit(ops, "remove", p, nil); err != nil {
				return err
			}
		case !inA:
			if err := emit(ops, "add", p, bv); err != nil {
				return err
			}
		default:
			if err := diffValue(p, av, bv, ops); err != nil {
				return err
			}
		}
	}
	return nil
}

func emit(ops *[]PatchOp, op, path string, v any) error {
	o := PatchOp{Op: op, Path: path}
	if op != "remove" {
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		o.Value = b
	}
	*ops = append(*ops, o)
	return nil
}

func escape(k string) string {
	return strings.ReplaceAll(strings.ReplaceAll(k, "~", "~0"), "/", "~1")
}
