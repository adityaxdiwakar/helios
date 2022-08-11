package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/getsentry/sentry-go"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport/http"
	influxdb2 "github.com/influxdata/influxdb-client-go/v2"
)

var (
	ledgerBinary string
	username     string
	authToken    string
	influxToken  string
	sentryToken  string
	consumerKey  string
	host         string

	sentryEnabled = true
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

	if sentryEnabled {
		// cause alert in sentry
		sentry.CaptureMessage(err.Error())
		sentry.Flush(2 * time.Second)
	}

	fmt.Println(err)
	os.Exit(code)
}

func init() {
	flag.StringVar(&ledgerBinary, "b", "ledger", "Ledger Binary")
	flag.StringVar(&username, "u", "", "Username")
	flag.StringVar(&authToken, "a", "", "Auth Token")
	flag.StringVar(&influxToken, "ia", "", "Influx Auth Token")
	flag.StringVar(&sentryToken, "st", "", "Sentry Token")
	flag.Parse()

	if username == "" || authToken == "" || influxToken == "" {
		bail(fmt.Errorf("must provide username and auth token"), 1)
	}

	if sentryToken == "" {
		sentryEnabled = false
	}

	if sentryEnabled {
		err := sentry.Init(sentry.ClientOptions{
			Dsn: sentryToken,
			// Set TracesSampleRate to 1.0 to capture 100%
			// of transactions for performance monitoring.
			// We recommend adjusting this value in production,
			TracesSampleRate: 1.0,
		})
		if err != nil {
			bail(fmt.Errorf("sentry.Init: %w", err), 1)
		}
	}
}

const (
	IraAccount = "Assets:Investments:IRA"
	TaxAccount = "Assets:Investments:Fidelity"
	Accounts   = "Assets:Investments"
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

	// 2> Generate quotes using tdaLedgerUpdate to prices.db
	if isMarketHours() {
		updatePriceDb()
	}

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

	// Calculate some profit fields
	gainsIra := marketIraOutput - basisIraOutput
	gainsTax := marketTaxOutput - basisTaxOutput

	// 4> Send data to local InfluxDB
	client := influxdb2.NewClient("http://localhost:8086", influxToken)
	writeApi := client.WriteAPI("primary", "primary")

	// 5> Get cost basis data for each security
	costBasisMap, err := getCostBasis(c.New(Accounts, "--average-lot-prices"))
	if err == nil {
		for ticker, basis := range *costBasisMap {
			value, err := returnSingleLine(
				c.New(
					"--price-db",
					"prices.db",
					"-V",
					fmt.Sprintf("Allocation:Equities:%s", strings.ToUpper(ticker)),
				),
			)
			if err != nil {
				continue
			}

			p := influxdb2.NewPointWithMeasurement("balance").
				AddTag("account", ticker).
				AddField("basis", basis).
				AddField("market", value).
				AddField("gain-percent", (value-basis)/basis)
			writeApi.WritePoint(p)
		}
	}

	p := influxdb2.NewPointWithMeasurement("balance").
		AddTag("account", "ira").
		AddField("basis", basisIraOutput).
		AddField("market", marketIraOutput).
		AddField("gain", gainsIra)
	writeApi.WritePoint(p)

	p = influxdb2.NewPointWithMeasurement("balance").
		AddTag("account", "tax").
		AddField("basis", basisTaxOutput).
		AddField("market", marketTaxOutput).
		AddField("gain", gainsTax)
	writeApi.WritePoint(p)

	// Force all unwritten data to be sent
	writeApi.Flush()
	// Ensures background processes finishes
	client.Close()
}

func getCostBasis(cmd *exec.Cmd) (*map[string]float64, error) {
	res := map[string]float64{}

	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("%s: %w", out, err)
	}

	costBody := strings.Split(string(out), "------\n")
	if len(costBody) < 2 {
		return nil, errors.New("could not parse cost basis command output")
	}
	lines := strings.Split(costBody[1], "\n")
	if len(lines) < 2 {
		return nil, errors.New("could not parse cost basis command output")
	}
	lines = lines[1 : len(lines)-1]

	for _, line := range lines {
		values := strings.Split(line, " ")
		if len(values) < 3 {
			return nil, errors.New("could not parse cost basis command output")
		}
		quantity, err := strconv.ParseFloat(values[0], 10)
		if err != nil {
			return nil, errors.New("could not parse cost basis command output")
		}
		ticker := values[1]
		basis, err := strconv.ParseFloat(values[2][2:len(values[2])-1], 10)
		if err != nil {
			return nil, errors.New("could not parse cost basis command output")
		}

		res[ticker] = quantity * basis
	}

	return &res, nil
}

func isMarketHours() bool {
	switch time.Now().UTC().Weekday() {
	case time.Saturday, time.Sunday:
	default:
		hr, _, _ := time.Now().Clock()
		// PST hours
		return hr > 1 && hr < 17
	}
	return false
}

func updatePriceDb() {
	cmd := exec.Command("tdaLedgerUpdate",
		"-f", "repo/records.ldg",
		"-p", "prices.db",
		"-b", "ledger",
		"-afile", "token")
	if out, err := cmd.Output(); err != nil {
		bail(fmt.Errorf("%s %w", string(out), err), 1)
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

func returnSingleLine(cmd *exec.Cmd) (float64, error) {
	out, err := cmd.CombinedOutput()
	if err != nil {
		return 0, fmt.Errorf("%s: %w", out, err)
	}

	line := strings.TrimSpace(string(out))
	line = strings.Split(line, " ")[0][1:]
	line = strings.ReplaceAll(line, ",", "")
	s, err := strconv.ParseFloat(line, 10)
	if err != nil {
		return 0, err
	}
	return s, nil
}

func returnLineSummary(cmd *exec.Cmd) (float64, error) {
	out, err := cmd.CombinedOutput()
	if err != nil {
		return 0, fmt.Errorf("%s: %w", out, err)
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
