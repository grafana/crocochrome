package psutil

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type Stats struct {
	RSSBytes int64
}

func (s Stats) Aggregate(other Stats) Stats {
	s.RSSBytes += other.RSSBytes

	return s
}

// PollStats periodically polls procfs for pid and its descendants, aggregating stats for all of them. The returned channel
// will be sent what PollStats considers valid snapshots of the process tree, that is, where no errors traversing it were
// detected. When `pid` stops existing, PollStats closes the channel.
func PollStats(pid int) <-chan Stats {
	ch := make(chan Stats)
	go func() {
		defer close(ch)

		for {
			time.Sleep(100 * time.Millisecond)

			treeStats := make([]Stats, 0, 8)
			mainstats, err := StatsFor(pid)
			if err != nil {
				// Stats for the main PID are not readable, it is either gone or we lack permissions. Return.
				return
			}

			treeStats = append(treeStats, mainstats)
			ptree, err := Tree(pid)
			if err != nil {
				// Found an error traversing children, assuming process tree changed while we were scanning it. Retry.
				continue
			}

			for _, child := range ptree[1:] { // First item is the main process, specialcased above.
				stats, err := StatsFor(child)
				if err != nil {
					// Found an error fetching child stats, assuming process tree changed while we were scanning it. Retry.
					continue
				}

				treeStats = append(treeStats, stats)
			}

			aggregatedStats := Stats{}
			for _, stats := range treeStats {
				aggregatedStats = aggregatedStats.Aggregate(stats)
			}

			ch <- aggregatedStats
		}
	}()

	return ch
}

func StatsFor(pid int) (Stats, error) {
	mem, err := os.ReadFile(filepath.Join(procPath(pid), "mem"))
	if err != nil {
		return Stats{}, fmt.Errorf("reading stats: %w", err)
	}

	// TODO: This is a mock.
	rss, err := strconv.ParseInt(string(mem), 10, 64)
	if err != nil {
		return Stats{}, fmt.Errorf("parsing memory: %w", err)
	}

	return Stats{
		RSSBytes: rss,
	}, nil
}

// Tree returns a list of PIDs for pid and its children, recursively.
// The first PID on the returned slice is pid.
func Tree(pid int) ([]int, error) {
	pids := make([]int, 0, 8)

	// Add yourself.
	pids = append(pids, pid)

	// For each children, recurse.
	children, err := ChildrenOf(pid)
	if err != nil {
		return nil, err
	}
	for _, child := range children {
		childTree, err := Tree(child)
		if err != nil {
			return nil, err
		}

		pids = append(pids, childTree...)
	}

	return pids, nil
}

// ChildrenOf returns the immediate children of pid. Grandchildren and pid itself are not returned, and thus the
// returned slice can be empty for a process that do not have children.
func ChildrenOf(pid int) ([]int, error) {
	children := make([]int, 0, 8)
	err := fs.WalkDir(os.DirFS(procPath(pid)), "task", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		taskChildren, err := os.ReadFile(filepath.Join(path, "children"))
		if err != nil {
			return fmt.Errorf("reading children file: %w", err)
		}

		for _, childStr := range strings.Split(string(taskChildren), " ") {
			child, err := strconv.Atoi(childStr)
			if err != nil {
				return fmt.Errorf("parsing pid %q: %w", childStr, err)
			}

			children = append(children, int(child))
		}

		return nil
	})

	return children, err
}

func procPath(pid int) string {
	return filepath.Join("/", "proc", strconv.Itoa(pid))
}
