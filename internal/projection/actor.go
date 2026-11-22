package projection

// ActorScope is derived from verified API authorization. Assignment and owner
// predicates are applied before report filters, aggregation, and row limits so
// a query cannot widen actor visibility.
type ActorScope struct {
	SubjectID      string
	OwnerOnly      bool
	OwnerField     string
	AssignedFields []string
}
