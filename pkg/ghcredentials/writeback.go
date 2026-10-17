package ghcredentials

import "gopkg.in/yaml.v3"

// Categories are a fixed allowlist: extension paths, aliases, account names,
// arbitrary configuration keys and values must never enter diagnostics.
type writebackSummary struct {
	Restorations   []hostRestoration      `json:"restorations,omitempty"`
	UnchangedFiles int                    `json:"unchanged_files"`
	WrittenFiles   int                    `json:"written_files"`
	Changes        map[string]changeCount `json:"changes,omitempty"`
}

type hostRestoration struct {
	Host    string `json:"host"`
	Outcome string `json:"outcome"`
}

func (s *writebackSummary) unchanged() {
	if s != nil {
		s.UnchangedFiles++
	}
}

func (s *writebackSummary) written(err error) {
	if s != nil && err == nil {
		s.WrittenFiles++
	}
}

type changeCount struct {
	Added          int `json:"added"`
	Modified       int `json:"modified"`
	Deleted        int `json:"deleted"`
	Conflicts      int `json:"current_wins"`
	AlreadyApplied int `json:"already_applied"`
}

func (s *writebackSummary) inspect(name string, before, after, latest []byte) error {
	if s == nil {
		return nil
	}
	if name != configFile && name != hostsFile {
		s.record("other_files", scalarBytes(before), scalarBytes(after), scalarBytes(latest))
		return nil
	}
	nodes, err := decodeConfigs(before, after, latest)
	if err != nil {
		return err
	}
	b, a, l := nodes[0], nodes[1], nodes[2]
	if equal(b, a) {
		s.UnchangedFiles++
		return nil
	}
	for key := range mappingKeys(b, a) {
		s.inspectNodes(changeCategory(name, key), get(b, key), get(a, key), get(l, key))
	}
	return nil
}

func decodeConfigs(configs ...[]byte) ([]*yaml.Node, error) {
	nodes := make([]*yaml.Node, 0, len(configs))
	for _, data := range configs {
		n, err := decode(data)
		if err != nil {
			return nil, err
		}
		nodes = append(nodes, n)
	}
	return nodes, nil
}

func changeCategory(name, key string) string {
	if name == hostsFile {
		return "hosts"
	}
	switch key {
	case "editor", "pager", "aliases", "git_protocol", "prompt", "browser":
		return "config." + key
	default:
		return "config.other"
	}
}

func scalarBytes(b []byte) *yaml.Node {
	if b == nil {
		return nil
	}
	return scalar(string(b))
}

func (s *writebackSummary) inspectNodes(category string, before, after, latest *yaml.Node) {
	if equal(before, after) {
		return
	}
	if mappingCompatible(before, after) {
		if before == nil {
			before = mapping()
		}
		for key := range mappingKeys(before, after) {
			s.inspectNodes(category, get(before, key), get(after, key), get(latest, key))
		}
		return
	}
	s.record(category, before, after, latest)
}

func (s *writebackSummary) record(category string, before, after, latest *yaml.Node) {
	if equal(before, after) {
		return
	}
	if s.Changes == nil {
		s.Changes = map[string]changeCount{}
	}
	c := s.Changes[category]
	switch {
	case before == nil:
		c.Added++
	case after == nil:
		c.Deleted++
	default:
		c.Modified++
	}
	if equal(after, latest) {
		c.AlreadyApplied++
	} else if !equal(before, latest) {
		c.Conflicts++
	}
	s.Changes[category] = c
}
