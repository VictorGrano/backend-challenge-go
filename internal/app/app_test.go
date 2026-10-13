package app

import (
	"testing"

	"go.uber.org/fx"
)

func TestModuleGraphIsValid(t *testing.T) {
	if err := fx.ValidateApp(Module); err != nil {
		t.Fatal(err)
	}
}
