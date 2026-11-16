// Parsed, never compiled: one of each failure-path shape, for the golden and
// mutation tests.
package corpus

import (
	"context"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"os"
)

type FetchError struct{}

func (*FetchError) Error() string { return "fetch" }

var ErrNotFound = errors.New("not found")

// errpath: error FetchError
func fetch(id int) (int, error) { return id, nil }

func store(v int) error { return nil }

func MustLoad(path string) int { return 0 }

type Repo interface {
	Save(v int) error
}

type Service struct {
	repo Repo
}

func linear(id int) (int, error) {
	v, err := fetch(id)
	if err != nil {
		return 0, err
	}
	if err := store(v); err != nil {
		return 0, fmt.Errorf("store: %w", err)
	}
	return v, nil
}

func branches(id int, retry bool) (int, error) {
	if retry {
		v, err := fetch(id)
		if err != nil {
			return 0, ErrNotFound
		}
		return v, nil
	} else if id == 0 {
		return 0, &FetchError{}
	}
	return id, nil
}

func switches(kind string, v any) error {
	switch kind {
	case "a":
		return store(1)
	case "b":
		fallthrough
	case "c":
		panic("unreachable kind")
	}
	switch v.(type) {
	case int:
		return nil
	default:
	}
	return nil
}

func loops(ids []int, stop bool) (int, error) {
	total := 0
outer:
	for _, id := range ids {
		for i := 0; i < id; i++ {
			if stop {
				break outer
			}
			if i == 2 {
				continue
			}
			v, err := fetch(i)
			if err != nil {
				return 0, err
			}
			total += v
		}
	}
	return total, nil
}

func silent(id int) {
	_ = store(id)
	v, _ := fetch(id)
	store(v)
	// errpath: allow(no-swallow) best effort
	_ = store(0)
}

func panics(path string) int {
	cfg := MustLoad(path)
	if cfg == 0 {
		log.Fatalf("no config at %s", path)
	}
	if cfg < 0 {
		os.Exit(2)
	}
	return cfg
}

func closures(ids []int) error {
	each := func(id int) error {
		if _, err := fetch(id); err != nil {
			return err
		}
		return nil
	}
	for _, id := range ids {
		if err := each(id); err != nil {
			return err
		}
	}
	go func() {
		panic("in goroutine")
	}()
	return nil
}

func recovery(id int) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("recovered: %v", r)
		}
	}()
	v, err := fetch(id)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	// mutation: equivalent, the bare return below returns err as well
	if err != nil {
		return
	}
	_ = v
	return
}

func handled(ids []int) int {
	n := 0
	for _, id := range ids {
		v, err := fetch(id)
		if err != nil {
			log.Printf("skip %d: %v", id, err)
			continue
		}
		n += v
	}
	return n
}

func (s *Service) Put(v int) error {
	if err := s.repo.Save(v); err != nil {
		return err
	}
	s.repo.Save(v + 1)
	return nil
}

func selects(done chan struct{}, in chan int) error {
	for {
		select {
		case <-done:
			return nil
		case v := <-in:
			if err := store(v); err != nil {
				return err
			}
		}
	}
}

func gotos(n int) error {
again:
	if n > 0 {
		n--
		goto again
	}
	if n < 0 {
		goto fail
	}
	return nil
fail:
	return ErrNotFound
}

func elses(id int, fast bool) (int, error) {
	if fast {
		return id, nil
	} else {
		v, err := fetch(id)
		if err != nil {
			return 0, err
		}
		id = v
	}
	if id > 10 {
		panic("too big")
	} else {
		id++
	}
	return id, nil
}

func labels(rows [][]int, done chan struct{}, in chan int) error {
rows:
	for _, row := range rows {
		for _, v := range row {
			if v < 0 {
				continue rows
			}
			if v == 0 {
				break rows
			}
			if err := store(v); err != nil {
				return err
			}
		}
	}
wait:
	for {
		select {
		case <-done:
			break wait
		case v := <-in:
			if v < 0 {
				continue
			}
			if err := store(v); err != nil {
				return err
			}
		}
	}
scan:
	for i := 0; i < 3; i++ {
		switch i {
		case 0:
			continue scan
		case 1:
			break
		case 2:
			break scan
		}
		if err := store(i); err != nil {
			return err
		}
	}
	return nil
}

// A label on the innermost loop names where an unlabelled jump lands anyway.
func innermost(ids []int) error {
each:
	for _, id := range ids {
		if id < 0 {
			continue each
		}
		if id == 0 {
			break each
		}
		if err := store(id); err != nil {
			return err
		}
	}
	return nil
}

// The break leaves the select, which ends the loop body: the same as continue.
func drain(in chan int) error {
	for {
		select {
		case v := <-in:
			if v == 0 {
				return nil
			}
			if err := store(v); err != nil {
				return err
			}
		default:
			break
		}
	}
}

// The condition, and its possible panic, runs before every iteration.
func polls(path string) error {
	n := 0
	for MustLoad(path) > n {
		n++
	}
	for i := 0; MustLoad(path) > i; i++ {
		if err := store(i); err != nil {
			return err
		}
	}
	return nil
}

func passthrough(id int) (int, error) {
	v, err := fetch(id)
	return v, err
}

func forwarded(id int) (int, error) {
	return fetch(id)
}

// The handler records the error, so it reaches the trace: no no-trace finding.
func observed(ctx context.Context, ids []int) int {
	n := 0
	for _, id := range ids {
		v, err := fetch(id)
		if err != nil {
			slog.WarnContext(ctx, "skip", "id", id, "error", err)
			continue
		}
		n += v
	}
	return n
}
