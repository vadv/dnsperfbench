package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"math/rand"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/miekg/dns"
	"github.com/montanaflynn/stats"
	"github.com/olekukonko/tablewriter"
	"github.com/vadv/dnsperfbench/pkg/httpbench"
	"golang.org/x/sync/semaphore"
)

type arrayFlags []string

func (i *arrayFlags) String() string {
	return "my string representation"
}

func (i *arrayFlags) Set(value string) error {
	*i = append(*i, value)
	return nil
}

var resolvers arrayFlags

var (
	raw              = flag.Bool("r", false, "Output raw mode")
	version          = flag.Bool("version", false, "Print version and exit")
	httptest         = flag.String("httptest", "", "Specify a URL to test including protocol (http or https)")
	defaultResolvers = []string{}
	resolverNames    = map[string]string{
		"8.8.8.8":                "Google",
		"1.1.1.1":                "Cloudflare",
		"9.9.9.9":                "Quad9",
		"114.114.114.114":        "114dns",
		"119.29.29.29":           "DNSPod",
		"180.76.76.76":           "Baidu",
		"208.67.222.222":         "OpenDNS",
		"199.85.126.20":          "Norton",
		"185.228.168.168":        "Clean Browsing",
		"8.26.56.26":             "Comodo",
		"[2001:4860:4860::8888]": "Google",
		"[2606:4700:4700::1111]": "Cloudflare",
		"[2620:fe::fe]":          "Quad9",
		"[2620:0:ccc::2]":        "OpenDNS", //https://www.opendns.com/about/innovations/ipv6/
		"[2a0d:2a00:1::]":        "Clean Browsing",
	}
	//All answers must match these
	expectedanswers = map[string]struct{}{
		"138.197.54.54": struct{}{},
		"138.197.53.4":  struct{}{},
	}
	//Duration to signal fail
	failDuration = time.Second * 10
	hostnamesHIT = []string{"fixed.turbobytes.net.", "fixed2.turbobytes.net."}
	auths        = map[string]string{
		"NS1":         "tbrum3.com.",
		"Google":      "tbrum4.com.",
		"AWS Route53": "tbrum5.com.",
		"DNSimple":    "tbrum14.com.",
		"GoDaddy":     "tbrum2.com.",
		"Akamai":      "tbrum9.com.",
		"Dyn":         "tbrum10.com.",
		"CloudFlare":  "tbrum8.com.",
		"EasyDNS":     "tbrum16.com.",
		"Ultradns":    "tbrum22.com.",
		"Azure":       "tbrum25.com.",
	}
	authSl          []string
	versionString   = "dirty"
	goVersionString = "unknown"
	workerLimit     *semaphore.Weighted
	queryLimit      *semaphore.Weighted
)

const (
	testrep = 15 //Number of times to repeat each test
)

func appendIfMissing(src []string, new string) []string {
	for _, ele := range src {
		if ele == new {
			return src
		}
	}
	return append(src, new)
}

func init() {
	for k := range resolverNames {
		defaultResolvers = append(defaultResolvers, k)
	}
	var tmp arrayFlags
	flag.Var(&tmp, "resolver", "Additional resolvers to test. default="+strings.Join(defaultResolvers, ", "))
	maxWorkers := flag.Int("workers", len(defaultResolvers), "Number of tests to run at once")
	maxQueries := flag.Int("queries", 5, "Limit the number of DNS queries in-flight at a time")
	flag.Parse()
	workerLimit = semaphore.NewWeighted(int64(*maxWorkers))
	queryLimit = semaphore.NewWeighted(int64(*maxQueries))
	resolvers = defaultResolvers
	for _, res := range tmp {
		resolvers = appendIfMissing(resolvers, res)
	}
	rand.Seed(time.Now().Unix())
	authSl = make([]string, 0)
	for auth := range auths {
		authSl = append(authSl, auth)
	}
	sort.Strings(authSl)
	if *version {
		fmt.Println(versionString)
		fmt.Println(goVersionString)
		os.Exit(0)
	}
}

var letterRunes = []rune("abcdefghijklmnopqrstuvwxyz")

func randStringRunes(n int) string {
	b := make([]rune, n)
	for i := range b {
		b[i] = letterRunes[rand.Intn(len(letterRunes))]
	}
	return string(b)
}

func testresolver(hostname, resolver string) (*time.Duration, error) {
	//Add to ratelimit, block until a slot is available
	if err := queryLimit.Acquire(context.TODO(), 1); err != nil {
		log.Fatal("Failed ta acquire semaphore", err)
		return nil, err
	}
	//Remove from rate limit when done
	defer queryLimit.Release(1)
	m := new(dns.Msg)
	m.Id = dns.Id()
	m.RecursionDesired = true
	m.SetQuestion(dns.Fqdn(hostname), dns.TypeA)
	c := new(dns.Client)
	//Life is too short to wait for DNS...
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	in, rtt, err := c.ExchangeContext(ctx, m, resolver+":53")
	if err != nil {
		return &failDuration, err
	}
	//Validate response
	//Expect only one answer
	if len(in.Answer) != 1 {
		return &failDuration, fmt.Errorf("Number of answers is not 1")
	}
	arec, ok := in.Answer[0].(*dns.A)
	if !ok {
		return &failDuration, fmt.Errorf("Answer is not type A")
	}
	_, ok = expectedanswers[arec.A.String()]
	if !ok {
		return &failDuration, fmt.Errorf("Got strange answer. Evil hijacking resolver?")
	}
	//rtt = rtt.Truncate(time.Millisecond / 4)
	return &rtt, nil
}

func runtests(host, res string, rndSuffix bool) resolverResults {
	//Actual test...
	vals := make([]time.Duration, 0)
	fails := 0
	for i := 0; i < testrep; i++ {
		hostname := host
		if rndSuffix {
			hostname = randStringRunes(15) + "." + host
		}
		rtt, err := testresolver(hostname, res)
		vals = append(vals, *rtt)
		if err != nil {
			fails++
		}
	}
	//Print summary
	//fmt.Printf("Failures: %v of 5\n", fails)
	//fmt.Printf("Timings: %v\n", vals)
	validVals := make([]float64, len(vals))
	for i, val := range vals {
		validVals[i] = float64(val)
	}
	median, _ := stats.Median(validVals)
	mean, _ := stats.Mean(validVals)
	return resolverResults{mean: time.Duration(mean), median: time.Duration(median), failratio: float64(fails) / testrep}
}

//SummaryResolver stores score for individual resolver
type SummaryResolver struct {
	Res   string
	Score float64
}

//Summary enables sorting slice of SummaryResolver
type Summary []SummaryResolver

func (a Summary) Len() int           { return len(a) }
func (a Summary) Swap(i, j int)      { a[i], a[j] = a[j], a[i] }
func (a Summary) Less(i, j int) bool { return a[i].Score < a[j].Score }

type resolverResults struct {
	mean      time.Duration
	median    time.Duration
	failratio float64
}

type recursiveResults map[string]resolverResults

func getms(dur time.Duration) float64 {
	return float64(dur) / float64(time.Millisecond)
}

func (res recursiveResults) Print(resolver, name string) {
	if *raw {
		result := res["ResolverHit"]
		fmt.Printf("Raw\t%s\tResolverHit\t%.2f\t%.2f\t%.2f\n", resolver, getms(result.mean), getms(result.median), result.failratio*100)
		for _, auth := range authSl {
			result := res[auth]
			fmt.Printf("Raw\t%s\t%s\t%.2f\t%.2f\t%.2f\n", resolver, auth, getms(result.mean), getms(result.median), result.failratio*100)
		}
	} else {
		fmt.Printf("========== %s (%s) ===========\n", resolver, name)
		table := tablewriter.NewWriter(os.Stdout)
		table.SetAutoWrapText(false)
		table.SetHeader([]string{"Auth", "Mean", "Median", "Fail"})
		result := res["ResolverHit"]
		table.Append([]string{"ResolverHit", result.mean.Round(time.Millisecond).String(), result.median.Round(time.Millisecond).String(), fmt.Sprintf("%.2f%%", result.failratio*100)})
		for _, auth := range authSl {
			result := res[auth]
			table.Append([]string{auth, result.mean.Round(time.Millisecond).String(), result.median.Round(time.Millisecond).String(), fmt.Sprintf("%.2f%%", result.failratio*100)})
		}
		table.Render()
	}
}

func (res recursiveResults) Score() float64 {
	result := res["ResolverHit"]
	score := 5 * (float64(result.mean/time.Millisecond) + float64(result.median/time.Millisecond))
	for _, auth := range authSl {
		result := res[auth]
		score += float64(result.mean/time.Millisecond) + float64(result.median/time.Millisecond)
	}
	return score
}

func testrecursive(res string) recursiveResults {
	results := make(map[string]resolverResults)
	hithost := hostnamesHIT[rand.Intn(len(hostnamesHIT))]
	//Prime the caches... ignoring results
	for i := 0; i < 5; i++ {
		testresolver(hithost, res)
	}
	results["ResolverHit"] = runtests(hithost, res, false)

	//Perform the auths
	for _, auth := range authSl {
		host := auths[auth]
		results[auth] = runtests(host, res, true)
	}
	return results
}

type resultoutput struct {
	recursive string
	result    recursiveResults
}

func main() {
	if *httptest != "" {
		//Means we are running http test instead of DNS
		u, err := url.Parse(*httptest)
		if err != nil {
			log.Fatal(err)
		}
		if u.Scheme != "http" && u.Scheme != "https" {
			log.Fatal("Only http:// and https:// schemes supported")
		}
		if u.Hostname() == "" {
			log.Fatal("Invalid URL")
		}
		results := httpbench.TestOverHTTP(u, resolvers)
		//Render Summary - no raw mode yet
		table := tablewriter.NewWriter(os.Stdout)
		table.SetAutoWrapText(false)
		table.SetHeader([]string{"Resolver", "Remote", "DNS", "Connect", "TLS", "TTFB", "Transfer", "TOTAL"})
		found := make(map[string]struct{})
		for _, res := range results {
			name := resolverNames[res.Server]
			if name == "" {
				name = "Unknown"
			}
			table.Append([]string{fmt.Sprintf("%s (%s)", res.Server, name), res.CI.Addr, res.CI.DNS.Round(time.Millisecond).String(), res.CI.Connect.Round(time.Millisecond).String(), res.CI.SSL.Round(time.Millisecond).String(), res.CI.TTFB.Round(time.Millisecond).String(), res.CI.Transfer.Round(time.Millisecond).String(), res.CI.Total.Round(time.Millisecond).String()})
			//Remove this server from resplver map
			found[res.Server] = struct{}{}
		}
		//For everything else stamp a FAIL
		for _, server := range resolvers {
			name := resolverNames[server]
			if name == "" {
				name = "Unknown"
			}
			_, ok := found[server]
			if !ok {
				table.Append([]string{fmt.Sprintf("%s (%s)", server, name), "FAIL", "FAIL", "FAIL", "FAIL", "FAIL", "FAIL", "FAIL"})
			}
		}
		table.Render()

		os.Exit(0)
	}

	resscore := make(map[string]float64)
	results := make(map[string]recursiveResults)
	resultschan := make(chan resultoutput, 1)

	// Respect worker limit.
	ctx := context.TODO()

	//Fire off tests
	for _, res := range resolvers {
		//Stagger the start of tests
		time.Sleep(time.Millisecond * 50)
		go func(recursive string) {
			if err := workerLimit.Acquire(ctx, 1); err != nil {
				log.Fatal("Failed ta acquire semaphore", err)
				return
			}
			log.Println("Issuing tests for ", recursive)
			defer workerLimit.Release(1)
			resultschan <- resultoutput{recursive: recursive, result: testrecursive(recursive)}
		}(res)
	}
	//Gather results
	for i := range resolvers {
		result := <-resultschan
		log.Printf("[%v/%v] Got results for %s\n", i+1, len(resolvers), result.recursive)
		results[result.recursive] = result.result
	}
	for _, res := range resolvers {
		name := resolverNames[res]
		if name == "" {
			name = "Unknown"
		}
		result := results[res]
		result.Print(res, name)
		resscore[res] = result.Score()
	}
	//Make slice
	var summary Summary = make([]SummaryResolver, 0)
	for k, v := range resscore {
		summary = append(summary, SummaryResolver{k, v})
	}
	sort.Sort(summary)
	if !*raw {
		fmt.Printf("========== Summary ===========\n")
		fmt.Println("Scores (lower is better)")
	}

	table := tablewriter.NewWriter(os.Stdout)
	table.SetAutoWrapText(false)
	table.SetHeader([]string{"Resolver", "Performance Score"})

	for _, sum := range summary {
		name := resolverNames[sum.Res]
		if name == "" {
			name = "Unknown"
		}
		table.Append([]string{fmt.Sprintf("%s (%s)", sum.Res, name), fmt.Sprintf("%.0f", sum.Score)})
		if *raw {
			fmt.Printf("Score\t%s\t%.0f\n", sum.Res, sum.Score)
		}
		//log.Println(sum.Res, sum.Score)
	}
	if *raw {
		fmt.Printf("Recommendation\t%s\n", summary[0].Res)
	} else {
		table.Render()
		fmt.Printf("You should probably use %s as your default resolver\n", summary[0].Res)
	}
}
