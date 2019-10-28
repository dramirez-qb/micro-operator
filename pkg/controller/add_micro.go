package controller

import (
	"github.com/micro/micro-operator/pkg/controller/micro"
)

func init() {
	// AddToManagerFuncs is a list of functions to create controllers and add them to a manager.
	AddToManagerFuncs = append(AddToManagerFuncs, micro.Add)
}
