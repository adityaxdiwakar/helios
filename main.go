package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport/http"
)

var (
	ledgerBinary string
	username     string
	authToken    string
)

type Cmd struct {
	Binary string
}

func NewBase(binary string) Cmd {
	return Cmd{Binary: binary}
}

func (c *Cmd) New(newargs ...string) *exec.Cmd {
	args := append([]string{"-f", "repo/records.ldg", "bal"}, newargs...)
	return exec.Command(c.Binary, args...)
}

func bail(err error, code int) {
	fmt.Println(err)
	os.Exit(code)
}

func init() {
	flag.StringVar(&ledgerBinary, "b", "ledger", "Ledger Binary")
	flag.StringVar(&username, "u", "", "Username")
	flag.StringVar(&authToken, "a", "", "Auth Token")
	flag.Parse()

	if username == "" || authToken == "" {
		bail(fmt.Errorf("must provide username and auth token"), 1)
	}
}

const (
	IraAccount = "Assets:Investments:IRA"
	TaxAccount = "Assets:Investments:Fidelity"
)

func main() {
	c := NewBase(ledgerBinary)

	// perform set up
	setupRepo()

	// 1> Read value from basis
	// .... ldgr -B bal Asset:Investments:Fidelity
	// .... ldgr -B bal Asset:Investments:IRA
	basisIraOutput, errIra := returnLineSummary(c.New("-B", IraAccount))
	basisTaxOutput, errTax := returnLineSummary(c.New("-B", TaxAccount))
	if errIra != nil {
		bail(errIra, 1)
	} else if errTax != nil {
		bail(errTax, 1)
	}
	totalBasisValue := basisIraOutput + basisTaxOutput

	// 2> Generate quotes using tdaLedgerUpdate to prices.db
	updatePriceDb()

	// 3> Read current market value
	// .... ldgr --price-db prices.db -V bal Asset:Investments:Fidelity
	// .... ldgr --price-db prices.db -V bal Asset:Investments:IRA
	marketIraOutput, errIra := returnLineSummary(c.New("--price-db", "prices.db", "-V", IraAccount))
	marketTaxOutput, errTax := returnLineSummary(c.New("--price-db", "prices.db", "-V", TaxAccount))
	if errIra != nil {
		bail(errIra, 1)
	} else if errTax != nil {
		bail(errTax, 1)
	}
	totalMarketValue := marketIraOutput + marketTaxOutput

	// Calculate some profit fields
	gainsIra := marketIraOutput - basisIraOutput
	gainsTax := marketTaxOutput - basisTaxOutput
	gainsTotal := totalMarketValue - totalBasisValue
	_, _, _ = gainsIra, gainsTax, gainsTotal

	// 4> Send data to local InfluxDB
}

func updatePriceDb() {
	cmd := exec.Command("tdaLedgerUpdate", "-f", "repo/records.ldg", "-p", "prices.db", "-b", "ledger")
	if _, err := cmd.Output(); err != nil {
		bail(err, 1)
	}
}

// sets up the ./repo/ directory to have an up to date records.ledger file
func setupRepo() {
	auth := http.BasicAuth{
		Username: username,
		Password: authToken,
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

func returnLineSummary(cmd *exec.Cmd) (float64, error) {
	out, err := cmd.Output()
	if err != nil {
		return 0, err
	}
	lines := strings.Split(string(out), "\n")
	if len(lines) < 4 {
		return 0, fmt.Errorf("len(lines) == %d, expected > 3", len(lines))
	}
	sum := strings.TrimSpace(lines[len(lines)-2])
	if len(sum) < 1 {
		return 0, fmt.Errorf("len(sum) == %d, expected > 1", len(sum))
	}
	sum = sum[1:]

	sum = strings.ReplaceAll(sum, ",", "")
	s, err := strconv.ParseFloat(sum, 10)
	if err != nil {
		return 0, err
	}
	return s, nil
}
