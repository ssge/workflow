package model

// First partition the interior nodes by split output. Every path must stop
// at the same first parallel join, using exactly one incoming flow per
// branch. Then prove each interior is single-entry and acyclic. The join's
// successor is deliberately outside this traversal (re-entry is legal).
func (p *Plan) pairRegions(file string, g topology) *Diagnostic {
	owner := make(map[string]string)
	fail := func(id, why string) *Diagnostic { return problem(InvalidModel, file, id, "parallelGateway", why) }
	for _, split := range p.data.Nodes {
		if split.Kind != ParallelGateway || len(split.outgoing) < 2 {
			continue
		}
		var join string
		arrivals := make(map[string]bool)
		for _, first := range g.out[split.ID] {
			branch := first.flow
			work := []arc{first}
			var members []string
			var arrival string
			for i := 0; i < len(work); i++ {
				edge := work[i]
				n := p.data.Nodes[p.nodes[edge.node]]
				if n.Kind == ParallelGateway {
					if len(n.outgoing) != 1 || (join != "" && join != n.ID) {
						return fail(n.ID, "branch reaches a nested split or a different join")
					}
					join = n.ID
					if arrival != "" && arrival != edge.flow {
						return fail(join, "branch reaches more than one join input")
					}
					arrival = edge.flow
					continue
				}
				if other, seen := owner[n.ID]; seen {
					if other != branch {
						return fail(n.ID, "overlapping parallel branches")
					}
					continue
				}
				owner[n.ID] = branch
				members = append(members, n.ID)
				if len(g.out[n.ID]) == 0 {
					return fail(n.ID, "parallel branch ends before join")
				}
				work = append(work, g.out[n.ID]...)
			}
			if arrival == "" || arrivals[arrival] {
				return fail(split.ID, "branches need distinct join inputs")
			}
			arrivals[arrival] = true
			// Kahn's algorithm counts only interior edges. A second entrance
			// (including an implicit timer exit) invalidates the region.
			degree := make(map[string]int, len(members))
			var ready []string
			for _, id := range members {
				for _, prev := range g.in[id] {
					if prev.flow == branch {
						continue
					}
					if owner[prev.node] != branch {
						return fail(id, "external entry into parallel branch")
					}
					degree[id]++
				}
				if degree[id] == 0 {
					ready = append(ready, id)
				}
			}
			for i := 0; i < len(ready); i++ {
				for _, edge := range g.out[ready[i]] {
					if edge.node == join {
						continue
					}
					degree[edge.node]--
					if degree[edge.node] == 0 {
						ready = append(ready, edge.node)
					}
				}
			}
			if len(ready) != len(members) {
				return fail(split.ID, "cycle inside parallel branch")
			}
		}
		if p.peers[join] != "" || len(arrivals) != len(g.in[join]) {
			return fail(join, "join inputs must come only from its matching split")
		}
		p.peers[split.ID], p.peers[join] = join, split.ID
	}
	for _, n := range p.data.Nodes {
		if n.Kind == ParallelGateway && p.peers[n.ID] == "" {
			return fail(n.ID, "unpaired parallel gateway")
		}
	}
	return nil
}
