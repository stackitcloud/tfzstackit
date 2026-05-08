package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"sort"
	"strings"
	"text/template" // nosemgrep: go.lang.security.audit.xss.import-text-template.import-text-template -- Terraform/HCL renderer, not HTML

	"github.com/miekg/dns"
	"golang.org/x/net/idna"
)

const (
	zoneTemplateStr = `resource "stackit_dns_zone" "{{ .ID }}" {
  project_id = var.project_id
  name       = "{{ .Domain }}"
  dns_name   = "{{ .Domain }}"
}
`
	recordTemplateStr = `{{- range .Record.Comments }}
# {{ . }}{{ end }}
resource "stackit_dns_record_set" "{{ .ResourceID }}" {
  project_id = var.project_id
  zone_id    = {{ zoneReference .ZoneID }}
  name       = "{{ .Record.Name }}"
  type       = "{{ .Record.Type }}"
  ttl        = "{{ .Record.TTL }}"
  records    = [{{ range $idx, $elem := .Record.Data }}{{ if $idx }}, {{ end }}{{ ensureQuoted $elem }}{{ end }}]
}
`
)

type syntaxMode uint8

func (m syntaxMode) String() string {
	switch m {
	case Modern:
		return "modern"
	case Legacy:
		return "legacy"
	default:
		panic("Unknown syntax")
	}
}

const (
	Modern syntaxMode = iota
	Legacy
)

type configGenerator struct {
	zoneTemplate   *template.Template
	recordTemplate *template.Template

	syntax syntaxMode
}

func newConfigGenerator(syntax syntaxMode) *configGenerator {
	g := &configGenerator{syntax: syntax}
	g.zoneTemplate = template.Must(template.New("zone").Parse(zoneTemplateStr))
	g.recordTemplate = template.Must(template.New("record").Funcs(template.FuncMap{
		"ensureQuoted":  ensureQuoted,
		"zoneReference": g.zoneReference,
	}).Parse(recordTemplateStr))
	return g
}

type zoneTemplateData struct {
	ID     string
	Domain string
}
type recordTemplateData struct {
	ResourceID string
	Record     dnsRecord
	ZoneID     string
}
type dnsRecord struct {
	Name     string
	Type     string
	TTL      uint32
	Data     []string
	Comments []string
}
type recordKey struct {
	Name string
	Type string
}
type recordKeySlice []recordKey

func (records recordKeySlice) Len() int {
	return len(records)
}
func (records recordKeySlice) Less(i, j int) bool {
	genKey := func(k recordKey) string {
		return fmt.Sprintf("%s-%s", k.Name, k.Type)
	}
	return genKey(records[i]) < genKey(records[j])
}
func (records recordKeySlice) Swap(i, j int) {
	tmp := records[i]
	records[i] = records[j]
	records[j] = tmp
}

var (
	excludedTypesRaw = flag.String("exclude", "SOA,NS", "Comma-separated list of record types to ignore")
	domain           = flag.String("domain", "", "Name of domain")
	zoneFile         = flag.String("zone-file", "", "Path to zone file. Defaults to <domain>.zone in working dir")
	legacySyntax     = flag.Bool("legacy-syntax", false, "Generate legacy terraform syntax (versions older than 0.12)")
)

func main() {
	flag.Parse()

	if *domain == "" {
		log.Fatal("Domain is required")
	}
	if *zoneFile == "" {
		*zoneFile = fmt.Sprintf("%s.zone", *domain)
	}

	excludedTypes := excludedTypesFromString(*excludedTypesRaw)

	fileReader, err := os.Open(*zoneFile)
	if err != nil {
		log.Fatal(err)
	}

	var syntax syntaxMode
	if !*legacySyntax {
		syntax = Modern
	} else {
		syntax = Legacy
	}
	g := newConfigGenerator(syntax)
	g.generateTerraformForZone(*domain, excludedTypes, fileReader, os.Stdout)
}

func (g *configGenerator) generateTerraformForZone(domain string, excludedTypes map[uint16]bool, zoneReader io.Reader, output io.Writer) {
	records := readZoneRecords(zoneReader, excludedTypes)

	zoneID, err := g.generateZoneResource(domain, output)
	if err != nil {
		log.Fatal(err)
	}

	recordKeys := make(recordKeySlice, 0, len(records))
	for key := range records {
		recordKeys = append(recordKeys, key)
	}
	sort.Sort(sort.Reverse(recordKeys))

	for _, key := range recordKeys {
		rec := records[key]
		err := g.generateRecordResource(rec, zoneID, output)
		if err != nil {
			log.Printf("Error: %v\n", err)
			continue
		}
	}
}

func readZoneRecords(zoneReader io.Reader, excludedTypes map[uint16]bool) map[recordKey]dnsRecord {
	records := make(map[recordKey]dnsRecord)
	zp := dns.NewZoneParser(zoneReader, *domain, *zoneFile)
	for rr, ok := zp.Next(); ok; rr, ok = zp.Next() {
		recordType := rr.Header().Rrtype
		if excludedTypes[recordType] {
			continue
		}

		record := generateRecord(rr, zp.Comment())

		key := recordKey{record.Name, record.Type}
		if _, ok := records[key]; ok {
			record = mergeRecords(records[key], record)
		}

		records[key] = record
	}
	if err := zp.Err(); err != nil {
		log.Printf("Error: %v\n", err)
	}
	return records
}

func (g *configGenerator) generateZoneResource(domain string, w io.Writer) (string, error) {
	zoneName := strings.TrimRight(domain, ".")
	data := zoneTemplateData{
		ID:     strings.ReplaceAll(zoneName, ".", "-"),
		Domain: zoneName,
	}

	err := g.zoneTemplate.Execute(w, data)
	return data.ID, err
}

func (g *configGenerator) generateRecordResource(record dnsRecord, zoneID string, w io.Writer) error {
	sanitizedName := sanitizeRecordName(record.Name)
	id := fmt.Sprintf("%s-%s", sanitizedName, record.Type)

	data := recordTemplateData{
		ResourceID: id,
		Record:     record,
		ZoneID:     zoneID,
	}

	return g.recordTemplate.Execute(w, data)
}

func mergeRecords(a, b dnsRecord) dnsRecord {
	a.Data = append(a.Data, b.Data...)
	a.Comments = append(a.Comments, b.Comments...)

	return a
}

func generateRecord(rr dns.RR, comment string) dnsRecord {
	header := rr.Header()
	name := strings.ToLower(header.Name)

	key := recordKey{
		Name: name,
		Type: dns.TypeToString[header.Rrtype],
	}

	data := strings.TrimPrefix(rr.String(), header.String())
	if key.Type == "CNAME" {
		data = strings.ToLower(data)
	}

	if key.Type == "TXT" {
		// Two formats the STACKIT API accepts for TXT record values:
		//   - Single chunk (≤255 chars): the bare content, e.g. v=spf1 mx -all
		//   - Multi chunk (>255 chars):  BIND multi-string form with each chunk
		//                                quoted and chunks separated by a space,
		//                                e.g. "v=DKIM1; k=rsa; " "p=MIIB..."
		// Long single-string values are rejected with "txt record content cannot
		// contain invalid characters". miekg/dns hands us each TXT in BIND text
		// form (always quoted), so we split on the chunk separator: a single
		// part means a short TXT and we strip the outer quotes; multiple parts
		// mean a long TXT and we keep the BIND form intact. %q then takes care
		// of HCL escaping in both cases.
		// Reference:
		// https://docs.stackit.cloud/products/network/core-networking/dns/how-tos/add-custom-and-long-txt-record/
		parts := strings.Split(data, `" "`)
		if len(parts) > 1 {
			data = fmt.Sprintf("%q", data)
		} else {
			data = fmt.Sprintf("%q", strings.TrimSuffix(strings.TrimPrefix(data, `"`), `"`))
		}
	}

	comments := make([]string, 0)
	if comment != "" {
		comments = append(comments, strings.TrimLeft(comment, ";"))
	}
	return dnsRecord{
		Name:     key.Name,
		Type:     key.Type,
		TTL:      header.Ttl,
		Data:     []string{data},
		Comments: comments,
	}
}

// sanitizeRecordName creates a normalized record name that Terraform accepts.
// Terraform only allows letters, numbers, dashes and underscores, while DNS
// records allow far more.
//  1. All dots are replaced with -
//  2. * is replaced by the string "wildcard"
//  3. IDN records are cleaned using punycode conversion
//  4. Any remaining non-allowed characters are replaced underscore
//  5. If the start of the record name is not a valid Terraform identifier,
//     then prepend an underscore.
func sanitizeRecordName(name string) string {
	withoutDots := strings.ReplaceAll(strings.TrimRight(name, "."), ".", "-")
	withoutAsterisk := strings.ReplaceAll(withoutDots, "*", "wildcard")

	punycoded, err := idna.Punycode.ToASCII(withoutAsterisk)
	if err != nil {
		log.Fatalf("Cannot create resource name from record %s: %v", name, err)
	}

	id := strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') ||
			(r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') ||
			(r == '-' || r == '_') {
			return r
		}
		return '_'
	}, punycoded)

	if (id[0] >= 'a' && id[0] <= 'z') ||
		(id[0] >= 'A' && id[0] <= 'Z') ||
		(id[0] == '_') {
		return id
	}

	return fmt.Sprintf("_%s", id)
}

func excludedTypesFromString(s string) map[uint16]bool {
	excludedTypes := make(map[uint16]bool)
	for _, t := range strings.Split(s, ",") {
		t = strings.ToUpper(t) // ensure upper case
		rrType := dns.StringToType[t]
		excludedTypes[rrType] = true
	}
	return excludedTypes
}

func ensureQuoted(s string) string {
	if s[0] == '"' && s[len(s)-1] == '"' {
		return s
	}
	return fmt.Sprintf("%q", s)
}

func (g *configGenerator) zoneReference(zone string) string {
	switch g.syntax {
	case Modern:
		return fmt.Sprintf("stackit_dns_zone.%s.zone_id", zone)
	case Legacy:
		return fmt.Sprintf(`"${stackit_dns_zone.%s.zone_id}"`, zone)
	default:
		panic(fmt.Sprintf("Unknown mode %v", g.syntax))
	}
}
