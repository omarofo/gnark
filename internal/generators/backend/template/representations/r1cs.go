package representations

// R1CS...
const R1CS = `

import (
	"errors"
	"fmt"
	"io"
	"math/big"

	"github.com/fxamacker/cbor/v2"

	"github.com/consensys/gnark/backend"
	"github.com/consensys/gnark/backend/r1cs/r1c"
	"github.com/consensys/gnark/internal/backend/ioutils"

	"github.com/consensys/gurvy"

	{{ template "import_fr" . }}
)

// R1CS decsribes a set of R1CS constraint
type R1CS struct {
	// Wires
	NbWires       uint64
	NbPublicWires uint64 // includes ONE wire
	NbSecretWires uint64
	SecretWires   []string // private wire names, correctly ordered (the i-th entry is the name of the (offset+)i-th wire)
	PublicWires   []string // public wire names, correctly ordered (the i-th entry is the name of the (offset+)i-th wire)
	Logs          []backend.LogEntry
	DebugInfo     []backend.LogEntry

	// Constraints
	NbConstraints   uint64 // total number of constraints
	NbCOConstraints uint64 // number of constraints that need to be solved, the first of the Constraints slice
	Constraints     []r1c.R1C
	Coefficients    []fr.Element // R1C coefficients indexes point here
}

// GetNbConstraints returns the total number of constraints
func (r1cs *R1CS) GetNbConstraints() uint64 {
	return r1cs.NbConstraints
}

// GetNbWires returns the number of wires
func (r1cs *R1CS) GetNbWires() uint64 {
	return r1cs.NbWires
}

// GetNbCoefficients return the number of unique coefficients needed in the R1CS
func (r1cs *R1CS) GetNbCoefficients() int {
	return len(r1cs.Coefficients)
}

// GetCurveID returns curve ID as defined in gurvy (gurvy.{{.Curve}})
func (r1cs *R1CS) GetCurveID() gurvy.ID {
	return gurvy.{{.Curve}}
}

// WriteTo encodes R1CS into provided io.Writer using cbor
func (r1cs *R1CS) WriteTo(w io.Writer) (int64, error) {
	_w := ioutils.WriterCounter{W: w} // wraps writer to count the bytes written
	encoder := cbor.NewEncoder(&_w)

	// encode our object
	err := encoder.Encode(r1cs)
	return _w.N, err
}

// ReadFrom attempts to decode R1CS from io.Reader using cbor
func (r1cs *R1CS) ReadFrom(r io.Reader) (int64, error) {
	decoder := cbor.NewDecoder(r)

	err := decoder.Decode(r1cs)
	return int64(decoder.NumBytesRead()), err
}

// IsSolved returns nil if given assignment solves the R1CS and error otherwise
// this method wraps r1cs.Solve() and allocates r1cs.Solve() inputs
func (r1cs *R1CS) IsSolved(assignment map[string]interface{}) error {
	a := make([]fr.Element, r1cs.NbConstraints)
	b := make([]fr.Element, r1cs.NbConstraints)
	c := make([]fr.Element, r1cs.NbConstraints)
	wireValues := make([]fr.Element, r1cs.NbWires)
	return r1cs.Solve(assignment, a, b, c, wireValues)
}

// Solve sets all the wires and returns the a, b, c vectors.
// the r1cs system should have been compiled before. The entries in a, b, c are in Montgomery form.
// assignment: map[string]value: contains the input variables
// a, b, c vectors: ab-c = hz
// wireValues =  [intermediateVariables | privateInputs | publicInputs]
func (r1cs *R1CS) Solve(assignment map[string]interface{}, a, b, c, wireValues []fr.Element) error {
	// compute the wires and the a, b, c polynomials
	if len(a) != int(r1cs.NbConstraints) || len(b) != int(r1cs.NbConstraints) || len(c) != int(r1cs.NbConstraints) || len(wireValues) != int(r1cs.NbWires) {
		return errors.New("invalid input size: len(a, b, c) == r1cs.NbConstraints and len(wireValues) == r1cs.NbWires")
	}

	// keep track of wire that have a value
	wireInstantiated := make([]bool, r1cs.NbWires)

	// instantiate the public/ private inputs
	// note that currently, there is a convertion from interface{} to fr.Element for each entry in the
	// assignment map. It can cost a SetBigInt() which converts from Regular ton Montgomery rep (1 mul)
	// while it's unlikely to be noticeable compared to the FFT and the MultiExp compute times,
	// there should be a faster (statically typed) path
	instantiateInputs := func(offset int, inputNames []string) error {
		for i := 0; i < len(inputNames); i++ {
			name := inputNames[i]
			if name == backend.OneWire {
				wireValues[i+offset].SetOne()
				wireInstantiated[i+offset] = true
			} else {
				if val, ok := assignment[name]; ok {
					wireValues[i+offset].SetInterface(val)
					wireInstantiated[i+offset] = true
				} else {
					return fmt.Errorf("%q: %w", name, backend.ErrInputNotSet)
				}
			}
		}
		return nil
	}
	// instantiate private inputs
	if r1cs.NbSecretWires != 0 {
		offset := int(r1cs.NbWires - r1cs.NbPublicWires - r1cs.NbSecretWires) // private input start index
		if err := instantiateInputs(offset, r1cs.SecretWires); err != nil {
			return err
		}
	}
	// instantiate public inputs
	{
		offset := int(r1cs.NbWires - r1cs.NbPublicWires) // public input start index
		if err := instantiateInputs(offset, r1cs.PublicWires); err != nil {
			return err
		}
	}

	// now that we know all inputs are set, defer log printing once all wireValues are computed
	// (or sooner, if a constraint is not satisfied)
	defer r1cs.printLogs(wireValues, wireInstantiated)

	// check if there is an inconsistant constraint
	var check fr.Element

	// Loop through computational constraints (the one wwe need to solve and compute a wire in)
	for i := 0; i < int(r1cs.NbCOConstraints); i++ {

		// solve the constraint, this will compute the missing wire of the gate
		r1cs.solveR1C(&r1cs.Constraints[i], wireInstantiated, wireValues)

		// at this stage we are guaranteed that a[i]*b[i]=c[i]
		// if not, it means there is a bug in the solver
		a[i], b[i], c[i] = instantiateR1C(&r1cs.Constraints[i], r1cs, wireValues)

		check.Mul(&a[i], &b[i])
		if !check.Equal(&c[i]) {
			panic("error solving r1c: " + a[i].String() + "*" + b[i].String() + "=" + c[i].String())
		}
	}

	// Loop through the assertions -- here all wireValues should be instantiated
	// if a[i] * b[i] != c[i]; it means the constraint is not satisfied
	for i := int(r1cs.NbCOConstraints); i < len(r1cs.Constraints); i++ {

		// A this stage we are not guaranteed that a[i+sizecg]*b[i+sizecg]=c[i+sizecg] because we only query the values (computed
		// at the previous step)
		a[i], b[i], c[i] = instantiateR1C(&r1cs.Constraints[i], r1cs, wireValues)

		// check that the constraint is satisfied
		check.Mul(&a[i], &b[i])
		if !check.Equal(&c[i]) {
			debugInfo := r1cs.DebugInfo[i-int(r1cs.NbCOConstraints)]
			debugInfoStr := r1cs.logValue(debugInfo, wireValues, wireInstantiated)
			return fmt.Errorf("%w: %s", backend.ErrUnsatisfiedConstraint, debugInfoStr)
		}
	}

	return nil
}

func (r1cs *R1CS) logValue(entry backend.LogEntry, wireValues []fr.Element, wireInstantiated []bool) string {
	var toResolve []interface{}
	for j := 0; j < len(entry.ToResolve); j++ {
		wireID := entry.ToResolve[j]
		if !wireInstantiated[wireID] {
			panic("wire values was not instantiated")
		}
		toResolve = append(toResolve, wireValues[wireID].String())
	}
	return fmt.Sprintf(entry.Format, toResolve...)
}

func (r1cs *R1CS) printLogs(wireValues []fr.Element, wireInstantiated []bool) {

	// for each log, resolve the wire values and print the log to stdout
	for i := 0; i < len(r1cs.Logs); i++ {
		fmt.Print(r1cs.logValue(r1cs.Logs[i], wireValues, wireInstantiated))
	}
}

// AddTerm returns res += (value * term.Coefficient)
func (r1cs *R1CS) AddTerm(res *fr.Element, t r1c.Term, value fr.Element) *fr.Element {
	coeffValue := t.CoeffValue()
	switch coeffValue {
	case 1:
		return res.Add(res, &value)
	case -1:
		return res.Sub(res, &value)
	case 0:
		return res
	case 2:
		var buffer fr.Element
		buffer.Double(&value)
		return res.Add(res, &buffer)
	default:
		var buffer fr.Element
		buffer.Mul(&r1cs.Coefficients[t.CoeffID()], &value)
		return res.Add(res, &buffer)
	}
}

// mulWireByCoeff returns into.Mul(into, term.Coefficient)
func (r1cs *R1CS) mulWireByCoeff(res *fr.Element, t r1c.Term) *fr.Element {
	coeffValue := t.CoeffValue()
	switch coeffValue {
	case 1:
		return res
	case -1:
		return res.Neg(res)
	case 0:
		return res.SetZero()
	case 2:
		return res.Double(res)
	default:
		return res.Mul(res, &r1cs.Coefficients[t.CoeffID()])
	}
}

// compute left, right, o part of a r1cs constraint
// this function is called when all the wires have been computed
// it instantiates the l, r o part of a R1C
func instantiateR1C(r *r1c.R1C, r1cs *R1CS, wireValues []fr.Element) (a, b, c fr.Element) {

	for _, t := range r.L {
		r1cs.AddTerm(&a, t, wireValues[t.ConstraintID()])
	}

	for _, t := range r.R {
		r1cs.AddTerm(&b, t, wireValues[t.ConstraintID()])
	}

	for _, t := range r.O {
		r1cs.AddTerm(&c, t, wireValues[t.ConstraintID()])
	}

	return
}

// solveR1c computes a wire by solving a r1cs
// the function searches for the unset wire (either the unset wire is
// alone, or it can be computed without ambiguity using the other computed wires
// , eg when doing a binary decomposition: either way the missing wire can
// be computed without ambiguity because the r1cs is correctly ordered)
func (r1cs *R1CS) solveR1C(r *r1c.R1C, wireInstantiated []bool, wireValues []fr.Element) {

	switch r.Solver {

	// in this case we solve a R1C by isolating the uncomputed wire
	case r1c.SingleOutput:

		// the index of the non zero entry shows if L, R or O has an uninstantiated wire
		// the content is the ID of the wire non instantiated
		var loc uint8

		var a, b, c fr.Element
		var termToCompute r1c.Term

		processTerm := func(t r1c.Term, val *fr.Element, locValue uint8) {
			cID := t.ConstraintID()
			if wireInstantiated[cID] {
				r1cs.AddTerm(val, t, wireValues[cID])
			} else {
				if loc != 0 {
					panic("found more than one wire to instantiate")
				}
				termToCompute = t
				loc = locValue
			}
		}

		for _, t := range r.L {
			processTerm(t, &a, 1)
		}

		for _, t := range r.R {
			processTerm(t, &b, 2)
		}

		for _, t := range r.O {
			processTerm(t, &c, 3)
		}

		// ensure we found the unset wire
		if loc == 0 {
			// this wire may have been instantiated as part of moExpression already
			return
		}

		// we compute the wire value and instantiate it
		cID := termToCompute.ConstraintID()

		switch loc {
		case 1:
			if !b.IsZero() {
				wireValues[cID].Div(&c, &b).
					Sub(&wireValues[cID], &a)
				r1cs.mulWireByCoeff(&wireValues[cID], termToCompute)
			}
		case 2:
			if !a.IsZero() {
				wireValues[cID].Div(&c, &a).
					Sub(&wireValues[cID], &b)
				r1cs.mulWireByCoeff(&wireValues[cID], termToCompute)
			}
		case 3:
			wireValues[cID].Mul(&a, &b).
				Sub(&wireValues[cID], &c)
			r1cs.mulWireByCoeff(&wireValues[cID], termToCompute)
		}

		wireInstantiated[cID] = true

	// in the case the R1C is solved by directly computing the binary decomposition
	// of the variable
	case r1c.BinaryDec:

		// the binary decomposition must be called on the non Mont form of the number
		var n fr.Element
		for _, t := range r.O {
			r1cs.AddTerm(&n, t, wireValues[t.ConstraintID()])
		}
		n = n.ToRegular()

		nbBits := len(r.L)

		// cs.reduce() is non deterministic, so the variables are not sorted according to the bit position
		// this slice is i->value of the ithbit
		bitSlice := make([]uint64, nbBits)

		// binary decomposition of n
		var i, j int
		for i*64 < nbBits {
			j = 0
			for j < 64 && i*64+j < len(r.L) {
				bitSlice[i*64+j] = (n[i] >> uint(j)) & 1
				j++
			}
			i++
		}

		// log of c>0 where c is a power of 2
		quickLog := func(bi big.Int) int {
			var bCopy, zero big.Int
			bCopy.Set(&bi)
			res := 0
			for bCopy.Cmp(&zero) != 0 {
				bCopy.Rsh(&bCopy, 1)
				res++
			}
			res--
			return res
		}

		// affecting the correct bit to the correct variable
		for _, t := range r.L {
			cID := t.ConstraintID()
			coefID := t.CoeffID()
			coef := r1cs.Coefficients[coefID]
			var bcoef big.Int
			coef.ToBigIntRegular(&bcoef)
			ithBit := quickLog(bcoef)
			wireValues[cID].SetUint64(bitSlice[ithBit])
			wireInstantiated[cID] = true
		}

	default:
		panic("unimplemented solving method")
	}
}

`

// R1CSTests ...
const R1CSTests = `
import (
	{{ template "import_backend" . }}
	"bytes"
	"testing"
	"reflect"
	"github.com/consensys/gnark/internal/backend/circuits"
	"github.com/consensys/gurvy"
)
func TestSerialization(t *testing.T) {
	for name, circuit := range circuits.Circuits {
		t.Run(name, func(t *testing.T) {
			r1cs := circuit.R1CS.ToR1CS(gurvy.{{.Curve}})
			var buffer bytes.Buffer
			var err error
			var written, read int64
			written, err = r1cs.WriteTo(&buffer)
			if err != nil {
				t.Fatal(err)
			}
			var reconstructed {{ toLower .Curve}}backend.R1CS
			read , err = reconstructed.ReadFrom(&buffer)
			if err != nil {
				t.Fatal(err)
			}
			if written != read {
				t.Fatal("didn't read same number of bytes we wrote")
			}
			// compare both
			if !reflect.DeepEqual(r1cs, &reconstructed) {
				t.Fatal("round trip serialization failed")
			}
		})
	}
}
`
