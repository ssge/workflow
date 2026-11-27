package model

import (
	"fmt"
	"slices"
)

// link owns data. Resolve edges only against process nodes, never against the
// document-wide table used for XML ID uniqueness.
func link(file string, data planData, declared map[string]declarations) (*Plan, *Diagnostic) {
	p := &Plan{data: data, nodes: make(map[string]int), flows: make(map[string]int), boundaries: make(map[string]string), peers: make(map[string]string)}
	ids := make(map[string]bool)
	register := func(id, kind string) *Diagnostic {
		switch {
		case !validID(id):
			return problem(InvalidModel, file, "", "id", kind+" needs a non-empty ID without whitespace")
		case ids[id]:
			return problem(InvalidModel, file, id, "id", "duplicate ID")
		}
		ids[id] = true
		return nil
	}
	if d := register(data.ProcessKey, "process"); d != nil {
		return nil, d
	}
	var ends []string
	for i, n := range data.Nodes {
		if d := register(n.ID, string(n.Kind)); d != nil {
			return nil, d
		}
		p.nodes[n.ID] = i
		switch n.Kind {
		case StartEvent:
			if p.start != "" {
				return nil, problem(InvalidModel, file, n.ID, "startEvent", "more than one start event")
			}
			p.start = n.ID
		case EndEvent:
			ends = append(ends, n.ID)
		case BoundaryEvent, ScriptTask, BusinessRuleTask, UserTask, ExclusiveGateway, ParallelGateway:
		default:
			return nil, problem(UnsupportedFeature, file, n.ID, "kind", "unsupported flow node")
		}
	}
	if p.start == "" {
		return nil, problem(InvalidModel, file, data.ProcessKey, "startEvent", "missing none start event")
	}
	if len(ends) == 0 {
		return nil, problem(InvalidModel, file, data.ProcessKey, "endEvent", "missing none end event")
	}
	g := newTopology()
	for i, f := range data.Flows {
		if d := register(f.ID, "sequenceFlow"); d != nil {
			return nil, d
		}
		source, ok := p.nodes[f.SourceRef]
		if !ok {
			return nil, problem(InvalidModel, file, f.ID, "sourceRef", fmt.Sprintf("%q is not a node in this process", f.SourceRef))
		}
		target, ok := p.nodes[f.TargetRef]
		if !ok {
			return nil, problem(InvalidModel, file, f.ID, "targetRef", fmt.Sprintf("%q is not a node in this process", f.TargetRef))
		}
		p.flows[f.ID] = i
		g.connect(f.SourceRef, f.TargetRef, f.ID)
		s, t := &p.data.Nodes[source], &p.data.Nodes[target]
		s.outgoing = append(s.outgoing, f.ID)
		t.incoming = append(t.incoming, f.ID)
	}
	for _, n := range p.data.Nodes {
		refs := declared[n.ID]
		if !consistent(refs.incoming, n.incoming) {
			return nil, problem(InvalidModel, file, n.ID, "incoming", "declared incoming IDs differ from incident sequence flows")
		}
		if !consistent(refs.outgoing, n.outgoing) {
			return nil, problem(InvalidModel, file, n.ID, "outgoing", "declared outgoing IDs differ from incident sequence flows")
		}
		switch n.Kind {
		case BoundaryEvent:
			host, ok := p.nodes[n.AttachedToRef]
			if !ok || data.Nodes[host].Kind != UserTask || p.boundaries[n.AttachedToRef] != "" {
				return nil, problem(InvalidModel, file, n.ID, "attachedToRef", "expected a user task without another boundary event")
			}
			if n.DurationSeconds < 1 || n.DurationSeconds > 2147483647 {
				return nil, problem(InvalidModel, file, n.ID, "timeDuration", "duration out of range")
			}
			p.boundaries[n.AttachedToRef] = n.ID
			g.connect(n.AttachedToRef, n.ID, "")
			if len(n.incoming) != 0 {
				return nil, problem(InvalidModel, file, n.ID, "incoming", "sequence flow cannot enter a boundary event")
			}
			fallthrough
		case ScriptTask, BusinessRuleTask, UserTask:
			if len(n.outgoing) != 1 {
				return nil, problem(InvalidModel, file, n.ID, "outgoing", "expected one unconditional outgoing flow")
			}
		case ParallelGateway, ExclusiveGateway:
			if d := gatewayShape(file, n); d != nil {
				return nil, d
			}
		case StartEvent:
			if len(n.incoming) != 0 {
				return nil, problem(InvalidModel, file, n.ID, "incoming", "start event cannot have incoming flows")
			}
			if len(n.outgoing) != 1 {
				return nil, problem(InvalidModel, file, n.ID, "outgoing", "start event needs exactly one outgoing flow")
			}
		case EndEvent:
			if len(n.outgoing) != 0 {
				return nil, problem(InvalidModel, file, n.ID, "outgoing", "end event cannot have outgoing flows")
			}
		}
	}
	for _, f := range data.Flows {
		src := data.Nodes[p.nodes[f.SourceRef]]
		requiresCondition := src.Kind == ExclusiveGateway && len(src.outgoing) > 1 && f.ID != src.DefaultFlow
		if requiresCondition != (f.Condition != "") {
			return nil, problem(InvalidModel, file, f.ID, "conditionExpression", "conditions are required exactly on non-default outgoing flows of an exclusive split")
		}
	}
	fromStart := reachable([]string{p.start}, g.out)
	toEnd := reachable(ends, g.in)
	for _, n := range p.data.Nodes {
		if !fromStart[n.ID] {
			return nil, problem(InvalidModel, file, n.ID, "incoming", "node is unreachable from start")
		}
		if !toEnd[n.ID] {
			return nil, problem(InvalidModel, file, n.ID, "outgoing", "node has no path to an end")
		}
	}
	if d := p.pairRegions(file, g); d != nil {
		return nil, d
	}
	return p, nil
}

func gatewayShape(file string, n Node) *Diagnostic {
	in, out := len(n.incoming), len(n.outgoing)
	if in == 0 || out == 0 {
		return problem(InvalidModel, file, n.ID, "gatewayDirection", "gateway needs incoming and outgoing flows")
	}
	if n.Kind == ParallelGateway && !(in == 1 && out >= 2 || in >= 2 && out == 1) {
		return problem(InvalidModel, file, n.ID, "parallelGateway", "expected a pure split or join")
	}
	if n.DefaultFlow != "" && (out < 2 || !slices.Contains(n.outgoing, n.DefaultFlow)) {
		return problem(InvalidModel, file, n.ID, "default", "default must reference an outgoing flow of an exclusive split")
	}
	direction := "Unspecified"
	switch {
	case in > 1 && out > 1:
		direction = "Mixed"
	case out > 1:
		direction = "Diverging"
	case in > 1:
		direction = "Converging"
	}
	if n.GatewayDirection != "" && n.GatewayDirection != "Unspecified" && n.GatewayDirection != direction {
		return problem(InvalidModel, file, n.ID, "gatewayDirection", "declared direction disagrees with the graph")
	}
	return nil
}

// Omitting a list is allowed. A supplied list must be an exact set (no
// duplicates), but its order does not affect routing.
func consistent(declared, actual []string) bool {
	if declared == nil {
		return true
	}
	if len(declared) != len(actual) {
		return false
	}
	remaining := make(map[string]bool, len(actual))
	for _, id := range actual {
		remaining[id] = true
	}
	for _, id := range declared {
		if !remaining[id] {
			return false
		}
		delete(remaining, id)
	}
	return true
}

// Logical arcs include the implicit interrupting exit from host to timer;
// this is never reported as a sequenceFlow in the public adjacency lists.
type arc struct{ node, flow string }
type topology struct{ in, out map[string][]arc }

func newTopology() topology { return topology{make(map[string][]arc), make(map[string][]arc)} }
func (g topology) connect(from, to, flow string) {
	g.out[from] = append(g.out[from], arc{to, flow})
	g.in[to] = append(g.in[to], arc{from, flow})
}

func reachable(seeds []string, edges map[string][]arc) map[string]bool {
	seen := make(map[string]bool)
	queue := seeds
	for _, id := range seeds {
		seen[id] = true
	}
	for i := 0; i < len(queue); i++ {
		for _, edge := range edges[queue[i]] {
			next := edge.node
			if !seen[next] {
				seen[next] = true
				queue = append(queue, next)
			}
		}
	}
	return seen
}
