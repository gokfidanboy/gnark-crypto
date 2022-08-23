package gkr

import (
	"github.com/consensys/gnark-crypto/ecc/bn254/fr"
	"github.com/consensys/gnark-crypto/ecc/bn254/fr/polynomial"
	"github.com/consensys/gnark-crypto/ecc/bn254/fr/sumcheck"
)

// The goal is to prove/verify evaluations of many instances of the same circuit

// Gate must be a low-degree polynomial
type Gate interface {
	Evaluate(...fr.Element) fr.Element
	NumInput() int
	Degree() int
}

type Wire struct {
	Gate       *Gate
	Inputs     []*Wire
	NumOutputs int // number of other wires using it as input, not counting doubles (i.e. providing two inputs to the same gate counts as one). By convention, equal to 1 for output wires
}

type CircuitLayer []Wire

// TODO: Constructor so that user doesn't have to give layers explicitly.
type Circuit []CircuitLayer

// WireAssignment is assignment of values to the same wire across many instances of the circuit
type WireAssignment map[*Wire]polynomial.MultiLin

type Proof [][]sumcheck.Proof // for each layer, for each wire, a sumcheck (for each variable, a polynomial)

// A claim about the value of a wire
type claim struct {
	in  []fr.Element
	out fr.Element
}

type eqTimesGateEvalSumcheckLazyClaims struct {
	wire               *Wire
	evaluationPoints   [][]fr.Element
	claimedEvaluations []fr.Element
	manager            *claimsManager // WARNING: Circular references
}

func (e *eqTimesGateEvalSumcheckLazyClaims) ClaimsNum() int {
	return len(e.evaluationPoints)
}

func (e *eqTimesGateEvalSumcheckLazyClaims) VarsNum() int {
	return len(e.evaluationPoints[0])
}

func (e *eqTimesGateEvalSumcheckLazyClaims) CombinedSum(a fr.Element) fr.Element {
	evalsAsPoly := polynomial.Polynomial(e.claimedEvaluations)
	return evalsAsPoly.Eval(&a)
}

func (e *eqTimesGateEvalSumcheckLazyClaims) Degree(i int) int {
	return 1 + e.wire.Gate.Degree()
}

func (e *eqTimesGateEvalSumcheckLazyClaims) VerifyFinalEval(r []fr.Element, combinationCoeff fr.Element, purportedValue fr.Element, proof interface{}) bool {
	inputEvaluations := proof.([]fr.Element)
	e.manager.addForInput(e.wire, r, inputEvaluations)

	numClaims := len(e.evaluationPoints)

	evaluation := polynomial.EvalEq(e.evaluationPoints[numClaims-1], r)
	for i := numClaims - 2; i >= 0; i-- {
		evaluation.Mul(&evaluation, &combinationCoeff)
		eq := polynomial.EvalEq(e.evaluationPoints[i], r)
		evaluation.Add(&evaluation, &eq)
	}

	gateEvaluation := e.wire.Gate.Evaluate(inputEvaluations)
	evaluation.Mul(&evaluation, &gateEvaluation)

	return evaluation.Equal(&purportedValue)
}

type eqTimesGateEvalSumcheckClaims struct {
	wire               *Wire
	evaluationPoints   [][]fr.Element // x in the paper
	claimedEvaluations []fr.Element   // y in the paper
	manager            *claimsManager

	inputPreprocessors []polynomial.MultiLin // P_u in the paper, so that we don't need to pass along all the circuit's evaluations

	eq polynomial.MultiLin // ∑_i τ_i eq(x_i, -)
}

func (c *eqTimesGateEvalSumcheckClaims) Combine(combinationCoeff fr.Element) polynomial.Polynomial {

	eqLength := 1 << len(c.evaluationPoints[0])
	// initialize the eq tables
	c.eq = polynomial.Make(eqLength) //TODO: MakeLarge here?
	eqAsPoly := polynomial.Polynomial(c.eq)
	eqAsPoly.SetZero()

	newEq := polynomial.MultiLin(polynomial.Make(eqLength)) //TODO: MakeLarge here?
	newEq[0].SetOne()

	aI := newEq[0]
	for k := 0; k < len(c.eq); k++ { //TODO: parallelizable?
		// define eq_k = aᵏ eq(x_k1, ..., x_kn, *, ..., *) where x_ki are the evaluation points
		newEq.Eq(c.evaluationPoints[k])

		if !newEq[0].Equal(&aI) {
			panic("Incorrect assumption: Eq modifies newEq[0]")
		}

		eqAsPoly.Add(eqAsPoly, polynomial.Polynomial(newEq))

		if k+1 < len(c.eq) {
			newEq[0].Mul(&newEq[0], &combinationCoeff) //TODO: Test this. newEq[0] maybe not preserving value?
			aI = newEq[0]
		}
	}

	// from this point on the claim is a rather simple one: g = E(h) × R_v (P_u0(h), ...) where E and the P_ui are multilinear and R_v is of low-degree

	return c.computeGJ()
}

// computeSumAndStep returns sum = ∑_i m(1, i...) and step = ∑_i m(1, i...) - m(0, i...)
func computeSumAndStep(m polynomial.MultiLin) (sum fr.Element, step fr.Element) {
	sum = m[len(m)/2:].Sum()
	step = m[:len(m)/2].Sum()
	step.Sub(&sum, &step)
	return
}

// computeGJ: gⱼ = ∑_{0≤i<2ⁿ⁻ʲ} g(r₁, r₂, ..., rⱼ₋₁, Xⱼ, i...) = ∑_{0≤i<2ⁿ⁻ʲ} E(r₁, ..., X_j, i...) R_v( P_u0(r₁, ..., X_j, i...), ... ) where  E = ∑ eq_k
// the polynomial is represented by the evaluations g_j(1), g_j(2), ..., g_j(deg(g_j)).
func (c *eqTimesGateEvalSumcheckClaims) computeGJ() polynomial.Polynomial {
	degGJ := 1 + c.wire.Gate.Degree() // guaranteed to be no smaller than the actual deg(g_j)

	// Let f ∈ {E} ∪ {P_ui}. It is linear in X_j, so f(n) = n×(f(1) - f(0)) + f(0), and f(0), f(1) are easily computed from the bookkeeping tables
	ESum, EStep := computeSumAndStep(c.eq)

	puSum := polynomial.Polynomial(polynomial.Make(len(c.inputPreprocessors)))
	puStep := polynomial.Make(len(c.inputPreprocessors))

	for i := 0; i < len(puSum); i++ {
		puSum[i], puStep[i] = computeSumAndStep(c.inputPreprocessors[i])
	}

	evals := make([]fr.Element, degGJ)
	for i := 1; i < degGJ; i++ {
		puSumAsElements := []fr.Element(puSum)
		evals[i] = c.wire.Gate.Evaluate(puSumAsElements...)
		evals[i].Mul(&evals[i], &ESum)

		if i+1 < degGJ {
			ESum.Add(&ESum, &EStep)
			puSum.Add(puSum, puStep)
		}
	}
	return evals
}

// Next first folds the "preprocessing" and "eq" polynomials then compute the new g_j
func (c *eqTimesGateEvalSumcheckClaims) Next(element fr.Element) polynomial.Polynomial {
	c.eq.Fold(element)
	for i := 0; i < len(c.inputPreprocessors); i++ {
		c.inputPreprocessors[i].Fold(element)
	}
	return c.computeGJ()
}

func (c *eqTimesGateEvalSumcheckClaims) VarsNum() int {
	return len(c.evaluationPoints[0])
}

func (c *eqTimesGateEvalSumcheckClaims) ClaimsNum() int {
	return len(c.claimedEvaluations)
}

func (c *eqTimesGateEvalSumcheckClaims) ProveFinalEval(r []fr.Element) interface{} {
	//defer the proof, return list of claims
	evaluations := make([]fr.Element, len(c.inputPreprocessors))
	for i, puI := range c.inputPreprocessors {
		if len(puI) != 1 {
			panic("must be one") //TODO: Remove
		}
		evaluations[i] = puI[0]
		polynomial.Dump(puI)
	}
	// TODO: Make sure all is dumped
	polynomial.Dump(c.claimedEvaluations, c.eq)

	c.manager.addForInput(c.wire, r, evaluations)

	return evaluations
}

type claimsManager struct {
	claimsMap  map[*Wire]*eqTimesGateEvalSumcheckLazyClaims
	assignment WireAssignment
}

func newClaimsManager(c Circuit, assignment WireAssignment) (claims claimsManager) {
	claims.assignment = assignment

	for _, layer := range c {
		for i := 0; i < len(layer); i++ {
			wire := &layer[i]

			claims.claimsMap[wire] = &eqTimesGateEvalSumcheckLazyClaims{
				wire:               wire,
				evaluationPoints:   make([][]fr.Element, 0, wire.NumOutputs),
				claimedEvaluations: polynomial.Make(wire.NumOutputs),
			}
		}
	}
	return
}

func (m *claimsManager) add(wire *Wire, evaluationPoint []fr.Element, evaluation fr.Element) {
	claim := m.claimsMap[wire]
	i := len(claim.evaluationPoints)
	claim.claimedEvaluations[i] = evaluation
	claim.evaluationPoints = append(claim.evaluationPoints, evaluationPoint)
}

// addForInput claims regarding all inputs to the wire, all evaluated at the same point
func (m *claimsManager) addForInput(wire *Wire, evaluationPoint []fr.Element, evaluations []fr.Element) {
	wiresWithClaims := make(map[*Wire]struct{}) // In case the gate takes the same wire as input multiple times, one claim would suffice

	for inputI, inputWire := range wire.Inputs {
		if _, found := wiresWithClaims[inputWire]; !found { //skip repeated claims
			wiresWithClaims[inputWire] = struct{}{}
			m.add(inputWire, evaluationPoint, evaluations[inputI])
		}
	}
}

func (m *claimsManager) getLazyClaim(wire *Wire) *eqTimesGateEvalSumcheckLazyClaims {
	return m.claimsMap[wire]
}

func (m *claimsManager) getClaim(wire *Wire) *eqTimesGateEvalSumcheckClaims {
	lazy := m.claimsMap[wire]
	res := &eqTimesGateEvalSumcheckClaims{
		wire:               wire,
		evaluationPoints:   lazy.evaluationPoints,
		claimedEvaluations: lazy.claimedEvaluations,
	}

	res.inputPreprocessors = make([]polynomial.MultiLin, wire.NumOutputs)

	for inputI, inputW := range wire.Inputs {
		res.inputPreprocessors[inputI] = m.assignment[inputW].Clone() //will be edited later, so must be deep copied
	}
	return res
}

// Prove consistency of the claimed assignment
func Prove(c Circuit, assignment WireAssignment, transcript sumcheck.ArithmeticTranscript, challengeSeed []byte) Proof {
	claims := newClaimsManager(c, assignment)

	outLayer := c[0]

	proof := make(Proof, len(c))
	// firstChallenge called rho in the paper
	firstChallenge := sumcheck.NextFromBytes(transcript, challengeSeed, assignment[&c[0][0]].NumVars()) //TODO: Clean way to extract numVars

	for i := 0; i < len(outLayer); i++ {
		wire := &outLayer[i]
		claims.add(wire, firstChallenge, assignment[wire].Evaluate(firstChallenge))
	}

	for layerI, layer := range c {
		proof[layerI] = make([]sumcheck.Proof, len(layer))
		for wireI := 0; wireI < len(layer); wireI++ {
			wire := &layer[wireI]

			proof[layerI][wireI] = sumcheck.Prove(claims.getClaim(wire), transcript, nil)
		}
	}

	return proof
}

// Verify the consistency of the claimed output with the claimed input
// Unlike in Prove, the assignment argument need not be complete
func Verify(c Circuit, assignment WireAssignment, proof Proof, transcript sumcheck.ArithmeticTranscript, challengeSeed []byte) bool {
	claims := newClaimsManager(c, assignment)

	outLayer := c[0]

	firstChallenge := sumcheck.NextFromBytes(transcript, challengeSeed, assignment[&c[0][0]].NumVars()) //TODO: Clean way to extract numVars

	for i := 0; i < len(outLayer); i++ {
		wire := &outLayer[i]
		claims.add(wire, firstChallenge, assignment[wire].Evaluate(firstChallenge))
	}

	for layerI, layer := range c {

		for wireI := 0; wireI < len(layer); wireI++ {
			wire := &layer[wireI]
			claim := claims.getLazyClaim(wire)

			if !sumcheck.Verify(claim, proof[layerI][wireI], transcript, nil) {
				return false //TODO: Any polynomials to dump?
			}

		}
	}
	return true
}
