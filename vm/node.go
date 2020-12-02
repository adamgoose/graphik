package vm

import (
	"errors"
	"github.com/autom8ter/graphik/gen/go"
	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/checker/decls"
	"strings"
)

type DocVM struct {
	e *cel.Env
}

func NewDocVM() (*DocVM, error) {
	e, err := cel.NewEnv(
		cel.Declarations(
			decls.NewVar("doc", decls.NewMapType(decls.String, decls.Any)),
		),
	)
	if err != nil {
		return nil, err
	}
	return &DocVM{e: e}, nil
}

func (n *DocVM) Program(expression string) (cel.Program, error) {
	if expression == "" {
		return nil, errors.New("empty expression")
	}
	ast, iss := n.e.Compile(expression)
	if iss.Err() != nil {
		return nil, iss.Err()
	}
	return n.e.Program(ast)
}

func (n *DocVM) Programs(expressions []string) ([]cel.Program, error) {
	var programs []cel.Program
	for _, exp := range expressions {
		if exp == "" {
			continue
		}
		prgm, err := n.Program(exp)
		if err != nil {
			return nil, err
		}
		programs = append(programs, prgm)
	}
	return programs, nil
}

func (n *DocVM) Eval(doc *apipb.Doc, programs ...cel.Program) (bool, error) {
	if len(programs) == 0 || programs[0] == nil {
		return true, nil
	}
	var passes = true
	for _, program := range programs {
		out, _, err := program.Eval(map[string]interface{}{
			"doc": doc.AsMap(),
		})
		if err != nil {
			if strings.Contains(err.Error(), "no such key") {
				return false, nil
			}
			return false, err
		}
		if val, ok := out.Value().(bool); !ok || !val {
			passes = false
		}
	}
	return passes, nil
}
