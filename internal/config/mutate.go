package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// loadYAMLRoot reads the YAML config at cfgPath, unmarshals it into a yaml.Node,
// and returns the top-level mapping node. It centralises the read/unmarshal/
// document+mapping validation that all mutator functions (AddHost, AddRunner,
// RemoveRunner) need before modifying the tree.
func loadYAMLRoot(cfgPath string) (*yaml.Node, error) {
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		return nil, fmt.Errorf("reading config: %w", err)
	}

	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}
	if root.Kind != yaml.DocumentNode || len(root.Content) == 0 {
		return nil, fmt.Errorf("unexpected YAML structure")
	}
	top := root.Content[0]
	if top.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("config root is not a mapping")
	}
	return top, nil
}

// AddHost adds a host entry to the config file at cfgPath. It reads the existing
// YAML, appends the host, validates, and writes back.
func AddHost(cfgPath, name, addr, hostOS, arch string) error {
	top, err := loadYAMLRoot(cfgPath)
	if err != nil {
		return err
	}

	hostsNode := findMapValue(top, "hosts")
	if hostsNode == nil {
		keyNode := &yaml.Node{Kind: yaml.ScalarNode, Value: "hosts"}
		hostsNode = &yaml.Node{Kind: yaml.MappingNode}
		top.Content = append(top.Content, keyNode, hostsNode)
	}
	if hostsNode.Kind != yaml.MappingNode {
		return fmt.Errorf("hosts is not a mapping")
	}

	if existing := findMapValue(hostsNode, name); existing != nil {
		return fmt.Errorf("host %q already exists in config", name)
	}

	hostEntry := &yaml.Node{Kind: yaml.MappingNode}
	hostEntry.Content = append(hostEntry.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Value: "addr"},
		&yaml.Node{Kind: yaml.ScalarNode, Value: addr},
	)
	if hostOS != "" {
		hostEntry.Content = append(hostEntry.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Value: "os"},
			&yaml.Node{Kind: yaml.ScalarNode, Value: hostOS},
		)
	}
	if arch != "" {
		hostEntry.Content = append(hostEntry.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Value: "arch"},
			&yaml.Node{Kind: yaml.ScalarNode, Value: arch},
		)
	}

	hostsNode.Content = append(hostsNode.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Value: name},
		hostEntry,
	)

	return writeYAMLBack(cfgPath, top)
}

// AddRunnerOpts holds parameters for AddRunner.
type AddRunnerOpts struct {
	Name      string
	Repo      string
	Org       string
	Group     string
	Host      string
	Count     int
	Labels    []string
	Ephemeral bool
}

// AddRunner adds a runner entry to the config file at cfgPath.
func AddRunner(cfgPath, name, repo, hostName string, count int, labels []string) error {
	return AddRunnerFull(cfgPath, AddRunnerOpts{
		Name:   name,
		Repo:   repo,
		Host:   hostName,
		Count:  count,
		Labels: labels,
	})
}

// AddRunnerFull adds a runner entry with all options.
func AddRunnerFull(cfgPath string, opts AddRunnerOpts) error {
	top, err := loadYAMLRoot(cfgPath)
	if err != nil {
		return err
	}

	runnersNode := findMapValue(top, "runners")
	if runnersNode == nil {
		keyNode := &yaml.Node{Kind: yaml.ScalarNode, Value: "runners"}
		runnersNode = &yaml.Node{Kind: yaml.SequenceNode}
		top.Content = append(top.Content, keyNode, runnersNode)
	}
	if runnersNode.Kind != yaml.SequenceNode {
		return fmt.Errorf("runners is not a sequence")
	}

	entry := &yaml.Node{Kind: yaml.MappingNode}
	entry.Content = append(entry.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Value: "name"},
		&yaml.Node{Kind: yaml.ScalarNode, Value: opts.Name},
	)

	if opts.Repo != "" {
		entry.Content = append(entry.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Value: "repo"},
			&yaml.Node{Kind: yaml.ScalarNode, Value: opts.Repo},
		)
	}
	if opts.Org != "" {
		entry.Content = append(entry.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Value: "org"},
			&yaml.Node{Kind: yaml.ScalarNode, Value: opts.Org},
		)
	}
	if opts.Group != "" {
		entry.Content = append(entry.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Value: "group"},
			&yaml.Node{Kind: yaml.ScalarNode, Value: opts.Group},
		)
	}

	entry.Content = append(entry.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Value: "host"},
		&yaml.Node{Kind: yaml.ScalarNode, Value: opts.Host},
	)

	if opts.Count > 1 {
		entry.Content = append(entry.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Value: "count"},
			&yaml.Node{Kind: yaml.ScalarNode, Value: fmt.Sprintf("%d", opts.Count), Tag: "!!int"},
		)
	}

	if len(opts.Labels) > 0 {
		labelsSeq := &yaml.Node{Kind: yaml.SequenceNode, Style: yaml.FlowStyle}
		for _, l := range opts.Labels {
			labelsSeq.Content = append(labelsSeq.Content,
				&yaml.Node{Kind: yaml.ScalarNode, Value: l},
			)
		}
		entry.Content = append(entry.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Value: "labels"},
			labelsSeq,
		)
	}

	if opts.Ephemeral {
		entry.Content = append(entry.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Value: "ephemeral"},
			&yaml.Node{Kind: yaml.ScalarNode, Value: "true", Tag: "!!bool"},
		)
	}

	runnersNode.Content = append(runnersNode.Content, entry)

	return writeYAMLBack(cfgPath, top)
}

// RemoveRunner removes a runner entry from the config file at cfgPath by runner name.
func RemoveRunner(cfgPath, runnerName string) error {
	top, err := loadYAMLRoot(cfgPath)
	if err != nil {
		return err
	}

	runnersNode := findMapValue(top, "runners")
	if runnersNode == nil || runnersNode.Kind != yaml.SequenceNode {
		return fmt.Errorf("runners is not a sequence")
	}

	// Find and remove the runner entry
	found := false
	var newContent []*yaml.Node
	for i := 0; i < len(runnersNode.Content); i++ {
		entry := runnersNode.Content[i]
		if entry.Kind != yaml.MappingNode {
			newContent = append(newContent, entry)
			continue
		}
		nameNode := findMapValue(entry, "name")
		if nameNode == nil || nameNode.Value != runnerName {
			newContent = append(newContent, entry)
			continue
		}
		found = true
	}

	if !found {
		return fmt.Errorf("runner %q not found in config", runnerName)
	}

	runnersNode.Content = newContent
	return writeYAMLBack(cfgPath, top)
}

func findMapValue(mapping *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			return mapping.Content[i+1]
		}
	}
	return nil
}

func writeYAMLBack(cfgPath string, root *yaml.Node) error {
	out, err := yaml.Marshal(root)
	if err != nil {
		return fmt.Errorf("marshalling config: %w", err)
	}

	if err := os.WriteFile(cfgPath, out, 0o600); err != nil {
		return fmt.Errorf("writing config: %w", err)
	}

	if _, loadErr := Load(cfgPath); loadErr != nil {
		return fmt.Errorf("config invalid after modification: %w", loadErr)
	}

	return nil
}
