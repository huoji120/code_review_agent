package tools

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

type todoCreateArgs struct {
	Title    string `json:"title"`
	Priority string `json:"priority"`
}

type todoUpdateArgs struct {
	ID       int    `json:"id"`
	Title    string `json:"title"`
	Status   string `json:"status"`
	Priority string `json:"priority"`
}

func (a *todoUpdateArgs) UnmarshalJSON(data []byte) error {
	type alias struct {
		ID       interface{} `json:"id"`
		Title    string      `json:"title"`
		Status   string      `json:"status"`
		Priority string      `json:"priority"`
	}
	var raw alias
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	id, err := normalizeTodoID(raw.ID)
	if err != nil {
		return err
	}
	a.ID = id
	a.Title = raw.Title
	a.Status = raw.Status
	a.Priority = raw.Priority
	return nil
}

func normalizeTodoID(value interface{}) (int, error) {
	switch v := value.(type) {
	case float64:
		return int(v), nil
	case string:
		id, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			return 0, fmt.Errorf("invalid todo id: %q", v)
		}
		return id, nil
	case nil:
		return 0, nil
	default:
		return 0, fmt.Errorf("invalid todo id type %T", value)
	}
}

func (r *Registry) todoCreate(raw json.RawMessage) Result {
	args, err := decodeArgs[todoCreateArgs](raw)
	if err != nil {
		return Result{OK: false, Error: err.Error()}
	}
	if args.Title == "" {
		return Result{OK: false, Error: "title is required"}
	}
	if args.Priority == "" {
		args.Priority = "medium"
	}
	todo := Todo{ID: r.nextTodoID, Title: args.Title, Status: "pending", Priority: args.Priority}
	r.nextTodoID++
	r.todos = append(r.todos, todo)
	return Result{OK: true, Data: r.todos, Message: "todo created"}
}

func (r *Registry) todoUpdate(raw json.RawMessage) Result {
	args, err := decodeArgs[todoUpdateArgs](raw)
	if err != nil {
		return Result{OK: false, Error: err.Error()}
	}
	if args.Status == "done" {
		args.Status = "completed"
	}
	index, err := r.findTodoIndex(args)
	if err != nil {
		return Result{OK: false, Error: err.Error()}
	}
	if index >= 0 {
		i := index
		if args.Title != "" {
			r.todos[i].Title = args.Title
		}
		if args.Status != "" {
			r.todos[i].Status = args.Status
		}
		if args.Priority != "" {
			r.todos[i].Priority = args.Priority
		}
		return Result{OK: true, Data: r.todos, Message: "todo updated"}
	}
	return Result{OK: false, Error: "todo not found"}
}

func (r *Registry) findTodoIndex(args todoUpdateArgs) (int, error) {
	if args.ID > 0 {
		for i := range r.todos {
			if r.todos[i].ID == args.ID {
				return i, nil
			}
		}
		return -1, nil
	}

	title := strings.TrimSpace(args.Title)
	if title == "" {
		return -1, fmt.Errorf("id is required when title is not provided")
	}

	match := -1
	for i := range r.todos {
		if r.todos[i].Title != title {
			continue
		}
		if match >= 0 {
			return -1, fmt.Errorf("multiple todos match title %q; provide id", title)
		}
		match = i
	}
	return match, nil
}

func (r *Registry) Todos() []Todo {
	out := make([]Todo, len(r.todos))
	copy(out, r.todos)
	return out
}
