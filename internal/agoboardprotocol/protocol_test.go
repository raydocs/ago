package agoboardprotocol

import "testing"

func TestBoardValidateRejectsDuplicateIDsSelfLoopsAndCycles(t *testing.T) {
	base := Board{
		SchemaVersion: SchemaVersion,
		ID:            "board-1",
		Version:       1,
		Title:         "Work",
		Tasks: []Task{
			testTask("a"),
			testTask("b"),
			testTask("c"),
		},
	}

	tests := []struct {
		name   string
		mutate func(*Board)
	}{
		{name: "duplicate task id", mutate: func(board *Board) { board.Tasks = append(board.Tasks, testTask("a")) }},
		{name: "duplicate dependency id", mutate: func(board *Board) {
			board.Dependencies = []Dependency{{ID: "d", TaskID: "a", DependsOn: "b"}, {ID: "d", TaskID: "c", DependsOn: "b"}}
		}},
		{name: "duplicate edge", mutate: func(board *Board) {
			board.Dependencies = []Dependency{{ID: "d1", TaskID: "a", DependsOn: "b"}, {ID: "d2", TaskID: "a", DependsOn: "b"}}
		}},
		{name: "self loop", mutate: func(board *Board) {
			board.Dependencies = []Dependency{{ID: "d", TaskID: "a", DependsOn: "a"}}
		}},
		{name: "cycle", mutate: func(board *Board) {
			board.Dependencies = []Dependency{{ID: "d1", TaskID: "a", DependsOn: "b"}, {ID: "d2", TaskID: "b", DependsOn: "c"}, {ID: "d3", TaskID: "c", DependsOn: "a"}}
		}},
		{name: "duplicate attempt id", mutate: func(board *Board) {
			board.Attempts = []Attempt{{ID: "attempt", TaskID: "a", WorkerID: "worker", State: AttemptLeased}, {ID: "attempt", TaskID: "b", WorkerID: "worker", State: AttemptLeased}}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := cloneBoard(base)
			test.mutate(&candidate)
			if err := candidate.Validate(); err == nil {
				t.Fatal("invalid board was accepted")
			}
		})
	}
}

func TestDependencyCommandRejectsCycleWithoutMutatingBoard(t *testing.T) {
	board := createBoard(t)
	board = addTask(t, board, "a")
	board = addTask(t, board, "b")
	board = addDependency(t, board, "d1", "a", "b")
	before := cloneBoard(board)

	_, _, err := Apply(board, coordinatorCommand(board, CommandDependencyAdd, "cycle", func(command *Command) {
		command.Dependency = &DependencySpec{ID: "d2", TaskID: "b", DependsOn: "a"}
	}))
	if err == nil {
		t.Fatal("cyclic dependency was accepted")
	}
	if len(board.Dependencies) != len(before.Dependencies) {
		t.Fatal("rejected command mutated input board")
	}
}

func testTask(id string) Task {
	return Task{ID: id, Title: id, State: TaskPlanned, TerminalContract: TerminalContract{Outcome: "complete " + id, AcceptanceCriteria: []string{"verified"}}}
}
