package circuits

import (
	"github.com/consensys/gnark-crypto/ecc"
	"github.com/consensys/gnark/frontend"
)

// test circuit with no computational constraints
type noComputationCircuit struct {
	A frontend.Variable `gnark:",public"`
	B frontend.Variable
}

func (c *noComputationCircuit) Define(curveID ecc.ID, cs frontend.API) error {
	cs.AssertIsEqual(c.A, c.B)
	return nil
}

func init() {

	var circuit, good, bad noComputationCircuit

	good.A = (42)
	good.B = (42)

	bad.A = (42)
	bad.B = (43)

	addEntry("noComputationCircuit", &circuit, &good, &bad)
}
