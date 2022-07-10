package main

import (
	"fmt"
	"os"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport/http"
)

func bail(err error, code int) {
	fmt.Println(err)
	os.Exit(code)
}

func main() {
	// perform set up
	setupRepo()

	// Read from records.ldg file:
	// 1> Read value from basis
	// .... ldgr -B bal Asset:Investments:Fidelity
	// .... ldgr -B bal Asset:Investments:IRA
	// 2> Generate quotes using tdaLedgerUpdate to quotes/prices.db
	// 3> Read current market value
	// .... ldgr --price-db quotes/prices.db -V bal Asset:Investments:Fidelity
	// .... ldgr --price-db quotes/prices.db -V bal Asset:Investments:IRA
	// 4> Send data to local InfluxDB
}

// sets up the ./repo/ directory to have an up to date records.ledger file
func setupRepo() {
	auth := http.BasicAuth{
		Username: os.Getenv("USERNAME"),
		Password: os.Getenv("TOKEN"),
	}

	repo, err := git.PlainOpen("repo")
	if err != nil {
		if err == git.ErrRepositoryNotExists {
			repo, err = git.PlainClone("repo", false, &git.CloneOptions{
				URL:  "https://github.com/adityaxdiwakar/accounting",
				Auth: &auth,
			})
			if err != nil {
				bail(err, 1)
			}
		} else {
			bail(err, 1)
		}
	}

	w, err := repo.Worktree()
	if err != nil {
		bail(err, 1)
	}

	err = w.Pull(&git.PullOptions{
		ReferenceName: plumbing.ReferenceName("refs/heads/master"),
		Auth:          &auth,
	})

	if err != nil && err != git.NoErrAlreadyUpToDate {
		bail(err, 1)
	}
}
