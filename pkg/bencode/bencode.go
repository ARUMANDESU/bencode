package bencode

import (
	"fmt"
	"io"
	"sort"
)

/*
# string
5:string -> "string"
3:lol -> "lol"

# integer
prefixed with `i` followed by base 10 number and followed by `e`
i4e -> 4
i-3e -> -3
i0e -> 0

# list
prefixed with `l`, followed by elements and followed by `e`
l4:spam3:lole -> ["spam", "lol"]
l3:keki-1e3:bane -> ["kek", -1, "ban"]d3:cow3:moo4:spam4:eggse → {"cow": "moo", "spam": "eggs"}
l4:spami42el4:nestee d3:key5:valueee -> ["spam", 42, ["nest"], {"key": "value"}]

# dictionary
prefixed with `d`<key><value>`e`
d3:lol3:keke -> {"lol": "kek"}
d3:keki4ee -> {"kek": 4}
d3:cow3:moo4:spam4:eggse → {"cow": "moo", "spam": "eggs"}
*/

type Value interface{ bencode() }

type Str []byte
type Int int64
type List []Value
type Dict map[string]Value

func (Str) bencode()  {}
func (Int) bencode()  {}
func (List) bencode() {}
func (Dict) bencode() {}

func encode(w io.Writer, v Value) (err error) {
	switch v := v.(type) {
	case Str:
		_, err = fmt.Fprintf(w, "%d:%s", len(v), v)
	case Int:
		_, err = fmt.Fprintf(w, "i%de", v)
	case List:
		_, err = io.WriteString(w, "l")
		if err != nil {
			return err
		}
		for _, ll := range v {
			err = encode(w, ll)
			if err != nil {
				return err
			}
		}
		_, err = io.WriteString(w, "e")
		if err != nil {
			return err
		}
	case Dict:
		_, err = io.WriteString(w, "d")
		if err != nil {
			return err
		}
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if err = encode(w, Str(k)); err != nil {
				return err
			}
			if err = encode(w, v[k]); err != nil {
				return err
			}
		}
		_, err = io.WriteString(w, "e")
		if err != nil {
			return err
		}
	default:
		return fmt.Errorf("unknown bencode value %T", v)
	}
	return err
}
