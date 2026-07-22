package wire

import (
	"fmt"

	"github.com/fables-for-robots/jobs-iroh/resources"
)

// Class is one rung of the fixed size ladder a job requirement is rounded up
// to. The ladder is part of the wire contract: queue subjects embed the class,
// and a runner serves every class that fits inside its own size.
type Class string

// The ladder, ascending by (cpu, mem). cN = N cores (millicpu), mN = N GiB.
var Classes = []Class{
	"c0.2-m1",
	"c1-m1",
	"c1-m2",
	"c1-m4",
	"c2-m4",
	"c2-m8",
	"c2-m16",
	"c4-m8",
	"c4-m16",
}

// classRes maps every rung to its capacity.
var classRes = map[Class]resources.Resources{
	"c0.2-m1": {CPUMilli: 200, MemBytes: 1 << 30},
	"c1-m1":   {CPUMilli: 1000, MemBytes: 1 << 30},
	"c1-m2":   {CPUMilli: 1000, MemBytes: 2 << 30},
	"c1-m4":   {CPUMilli: 1000, MemBytes: 4 << 30},
	"c2-m4":   {CPUMilli: 2000, MemBytes: 4 << 30},
	"c2-m8":   {CPUMilli: 2000, MemBytes: 8 << 30},
	"c2-m16":  {CPUMilli: 2000, MemBytes: 16 << 30},
	"c4-m8":   {CPUMilli: 4000, MemBytes: 8 << 30},
	"c4-m16":  {CPUMilli: 4000, MemBytes: 16 << 30},
}

// Resources returns the rung's capacity. Unknown classes return zero.
func (c Class) Resources() resources.Resources { return classRes[c] }

// Valid reports whether c is a ladder rung.
func (c Class) Valid() bool { _, ok := classRes[c]; return ok }

// ClassFor rounds a requirement UP to the smallest rung that fits it. A
// requirement beyond the top rung is capped at the top rung (and the caller
// should log that the cap bound).
func ClassFor(req resources.Resources) (Class, bool) {
	for _, c := range Classes {
		r := classRes[c]
		if req.CPUMilli <= r.CPUMilli && req.MemBytes <= r.MemBytes {
			return c, true
		}
	}
	return Classes[len(Classes)-1], false
}

// ClassesWithin returns every rung a runner of the given size can serve
// (rung.cpu ≤ size.cpu AND rung.mem ≤ size.mem).
func ClassesWithin(size resources.Resources) []Class {
	var out []Class
	for _, c := range Classes {
		r := classRes[c]
		if r.CPUMilli <= size.CPUMilli && r.MemBytes <= size.MemBytes {
			out = append(out, c)
		}
	}
	return out
}

// ParseClass validates a --size flag value.
func ParseClass(s string) (Class, error) {
	c := Class(s)
	if !c.Valid() {
		return "", fmt.Errorf("unknown size class %q (ladder: %v)", s, Classes)
	}
	return c, nil
}
