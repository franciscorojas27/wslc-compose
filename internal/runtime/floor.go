package runtime

import (
	"fmt"
	"sort"
	"strings"
	"wslc-compose/internal/config"
)

func BuildFloors(services map[string]config.Service) ([][]string, error) {
	if len(services) == 0 {
		return nil, nil
	}

	inDegree := make(map[string]int, len(services))
	dependents := make(map[string][]string, len(services))

	for name := range services {
		inDegree[name] = 0
	}

	for name, svc := range services {
		for _, dep := range svc.DependsOn.SortedNames() {
			if dep == name {
				return nil, fmt.Errorf("service %q cannot depend on itself", name)
			}
			if _, ok := services[dep]; !ok {
				return nil, fmt.Errorf("service %q depends on undefined service %q", name, dep)
			}
			inDegree[name]++
			dependents[dep] = append(dependents[dep], name)
		}
	}

	var current []string
	for name, deg := range inDegree {
		if deg == 0 {
			current = append(current, name)
		}
	}
	sort.Strings(current)

	var floors [][]string
	processed := 0

	for len(current) > 0 {
		floors = append(floors, current)
		processed += len(current)

		var next []string
		for _, name := range current {
			for _, child := range dependents[name] {
				inDegree[child]--
				if inDegree[child] == 0 {
					next = append(next, child)
				}
			}
		}
		sort.Strings(next)
		current = next
	}

	if processed != len(services) {
		var cyclic []string
		for name, deg := range inDegree {
			if deg > 0 {
				cyclic = append(cyclic, name)
			}
		}
		sort.Strings(cyclic)
		return nil, fmt.Errorf("circular dependency detected involving services: %s", strings.Join(cyclic, ", "))
	}

	return floors, nil
}

func expandServiceDependencies(services map[string]config.Service, targets []string) ([]string, error) {
	if len(targets) == 0 {
		names := make([]string, 0, len(services))
		for name := range services {
			names = append(names, name)
		}
		sort.Strings(names)
		return names, nil
	}

	visited := make(map[string]bool)
	var visit func(string) error
	visit = func(name string) error {
		svc, ok := services[name]
		if !ok {
			return fmt.Errorf("service %q not found in compose file", name)
		}
		if visited[name] {
			return nil
		}
		visited[name] = true
		for _, dep := range svc.DependsOn.SortedNames() {
			if err := visit(dep); err != nil {
				return err
			}
		}
		return nil
	}

	for _, target := range targets {
		if err := visit(target); err != nil {
			return nil, err
		}
	}

	expanded := make([]string, 0, len(visited))
	for name := range visited {
		expanded = append(expanded, name)
	}
	sort.Strings(expanded)
	return expanded, nil
}
