package exoscale

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	egoscale "github.com/exoscale/egoscale/v2"

	"github.com/StackExchange/dnscontrol/v3/models"
	"github.com/StackExchange/dnscontrol/v3/pkg/diff"
	"github.com/StackExchange/dnscontrol/v3/pkg/diff2"
	"github.com/StackExchange/dnscontrol/v3/pkg/printer"
	"github.com/StackExchange/dnscontrol/v3/providers"
)

const (
	defaultAPIZone = "ch-gva-2"
)

// ErrDomainNotFound error indicates domain name is not managed by Exoscale.
var ErrDomainNotFound = errors.New("domain not found")

type exoscaleProvider struct {
	client  *egoscale.Client
	apiZone string
}

// NewExoscale creates a new Exoscale DNS provider.
func NewExoscale(m map[string]string, metadata json.RawMessage) (providers.DNSServiceProvider, error) {
	endpoint, apiKey, secretKey := m["dns-endpoint"], m["apikey"], m["secretkey"]

	client, err := egoscale.NewClient(
		apiKey,
		secretKey,
		egoscale.ClientOptWithAPIEndpoint(endpoint),
	)
	if err != nil {
		return nil, err
	}

	provider := exoscaleProvider{
		client:  client,
		apiZone: defaultAPIZone,
	}

	if z, ok := m["apizone"]; ok {
		provider.apiZone = z
	}

	return &provider, nil
}

var features = providers.DocumentationNotes{
	providers.CanGetZones:            providers.Unimplemented(),
	providers.CanUseAlias:            providers.Can(),
	providers.CanUseCAA:              providers.Can(),
	providers.CanUsePTR:              providers.Can(),
	providers.CanUseSRV:              providers.Can("SRV records with empty targets are not supported"),
	providers.CanUseTLSA:             providers.Cannot(),
	providers.DocCreateDomains:       providers.Cannot(),
	providers.DocDualHost:            providers.Cannot("Exoscale does not allow sufficient control over the apex NS records"),
	providers.DocOfficiallySupported: providers.Cannot(),
}

func init() {
	fns := providers.DspFuncs{
		Initializer:   NewExoscale,
		RecordAuditor: AuditRecords,
	}
	providers.RegisterDomainServiceProviderType("EXOSCALE", fns, features)
}

// EnsureDomainExists returns an error if domain doesn't exist.
func (c *exoscaleProvider) EnsureDomainExists(domainName string) error {
	_, err := c.findDomainByName(domainName)

	return err
}

// GetNameservers returns the nameservers for domain.
func (c *exoscaleProvider) GetNameservers(domain string) ([]*models.Nameserver, error) {
	return nil, nil
}

// GetZoneRecords gets the records of a zone and returns them in RecordConfig format.
func (c *exoscaleProvider) GetZoneRecords(domain string) (models.Records, error) {
	return nil, fmt.Errorf("not implemented")
	// This enables the get-zones subcommand.
	// Implement this by extracting the code from GetDomainCorrections into
	// a single function.  For most providers this should be relatively easy.
}

// GetDomainCorrections returns a list of corretions for the  domain.
func (c *exoscaleProvider) GetDomainCorrections(dc *models.DomainConfig) ([]*models.Correction, error) {
	dc.Punycode()

	domain, err := c.findDomainByName(dc.Name)
	if err != nil {
		return nil, err
	}

	domainID := *domain.ID

	ctx := context.Background()
	records, err := c.client.ListDNSDomainRecords(ctx, c.apiZone, domainID)
	if err != nil {
		return nil, err
	}

	existingRecords := make([]*models.RecordConfig, 0, len(records))
	for _, r := range records {
		if r.ID == nil {
			continue
		}

		recordID := *r.ID

		record, err := c.client.GetDNSDomainRecord(ctx, c.apiZone, domainID, recordID)
		if err != nil {
			return nil, err
		}

		// nil pointers are not expected, but just to be on the safe side...
		var rtype, rcontent, rname string
		if record.Type == nil {
			continue
		}
		rtype = *record.Type
		if record.Content != nil {
			rcontent = *record.Content
		}
		if record.Name != nil {
			rname = *record.Name
		}

		if rtype == "SOA" || rtype == "NS" {
			continue
		}
		if rname == "" {
			t := "@"
			record.Name = &t
		}
		if rtype == "CNAME" || rtype == "MX" || rtype == "ALIAS" || rtype == "SRV" {
			t := rcontent + "."
			// for SRV records we need to aditionally prefix target with priority, which API handles as separate field.
			if rtype == "SRV" && record.Priority != nil {
				t = fmt.Sprintf("%d %s", *record.Priority, t)
			}
			rcontent = t
		}
		// exoscale adds these odd txt records that mirror the alias records.
		// they seem to manage them on deletes and things, so we'll just pretend they don't exist
		if rtype == "TXT" && strings.HasPrefix(rcontent, "ALIAS for ") {
			continue
		}

		rc := &models.RecordConfig{
			Original: record,
		}
		if record.TTL != nil {
			rc.TTL = uint32(*record.TTL)
		}
		rc.SetLabel(rname, dc.Name)

		switch rtype {
		case "ALIAS", "URL":
			rc.Type = rtype
			rc.SetTarget(rcontent)
		case "MX":
			var prio uint16
			if record.Priority != nil {
				prio = uint16(*record.Priority)
			}
			err = rc.SetTargetMX(prio, rcontent)
		default:
			err = rc.PopulateFromString(rtype, rcontent, dc.Name)
		}
		if err != nil {
			return nil, fmt.Errorf("unparsable record received from exoscale: %w", err)
		}

		existingRecords = append(existingRecords, rc)
	}
	removeOtherNS(dc)

	// Normalize
	models.PostProcessRecords(existingRecords)

	var corrections []*models.Correction
	var create, delete, modify diff.Changeset
	if !diff2.EnableDiff2 {
		differ := diff.New(dc)
		_, create, delete, modify, err = differ.IncrementalDiff(existingRecords)
	} else {
		differ := diff.NewCompat(dc)
		_, create, delete, modify, err = differ.IncrementalDiff(existingRecords)
	}
	if err != nil {
		return nil, err
	}

	for _, del := range delete {
		record := del.Existing.Original.(*egoscale.DNSDomainRecord)
		corrections = append(corrections, &models.Correction{
			Msg: del.String(),
			F:   c.deleteRecordFunc(*record.ID, domainID),
		})
	}

	for _, cre := range create {
		rc := cre.Desired
		corrections = append(corrections, &models.Correction{
			Msg: cre.String(),
			F:   c.createRecordFunc(rc, domainID),
		})
	}

	for _, mod := range modify {
		old := mod.Existing.Original.(*egoscale.DNSDomainRecord)
		new := mod.Desired
		corrections = append(corrections, &models.Correction{
			Msg: mod.String(),
			F:   c.updateRecordFunc(old, new, domainID),
		})
	}

	return corrections, nil
}

// Returns a function that can be invoked to create a record in a zone.
func (c *exoscaleProvider) createRecordFunc(rc *models.RecordConfig, domainID string) func() error {
	return func() error {
		target := rc.GetTargetCombined()
		name := rc.GetLabel()
		var prio *int64

		if rc.Type == "MX" {
			target = rc.GetTargetField()

			if rc.MxPreference != 0 {
				p := int64(rc.MxPreference)
				prio = &p
			}
		}

		if rc.Type == "SRV" {
			// API wants priority as a separate argument, here we will strip it from combined target.
			sp := strings.Split(target, " ")
			target = strings.Join(sp[1:], " ")
			p, err := strconv.ParseInt(sp[0], 10, 64)
			if err != nil {
				return err
			}
			prio = &p
		}

		if rc.Type == "NS" && (name == "@" || name == "") {
			name = "*"
		}

		record := egoscale.DNSDomainRecord{
			Name:     &name,
			Type:     &rc.Type,
			Content:  &target,
			Priority: prio,
		}

		if rc.TTL != 0 {
			ttl := int64(rc.TTL)
			record.TTL = &ttl
		}

		_, err := c.client.CreateDNSDomainRecord(context.Background(), c.apiZone, domainID, &record)

		return err
	}
}

// Returns a function that can be invoked to delete a record in a zone.
func (c *exoscaleProvider) deleteRecordFunc(recordID, domainID string) func() error {
	return func() error {
		return c.client.DeleteDNSDomainRecord(
			context.Background(),
			c.apiZone,
			domainID,
			&egoscale.DNSDomainRecord{ID: &recordID},
		)
	}
}

// Returns a function that can be invoked to update a record in a zone.
func (c *exoscaleProvider) updateRecordFunc(record *egoscale.DNSDomainRecord, rc *models.RecordConfig, domainID string) func() error {
	return func() error {
		target := rc.GetTargetCombined()
		name := rc.GetLabel()

		if rc.Type == "MX" {
			target = rc.GetTargetField()

			if rc.MxPreference != 0 {
				p := int64(rc.MxPreference)
				record.Priority = &p
			}
		}

		if rc.Type == "SRV" {
			// API wants priority as separate argument, here we will strip it from combined target.
			sp := strings.Split(target, " ")
			target = strings.Join(sp[1:], " ")
			p, err := strconv.ParseInt(sp[0], 10, 64)
			if err != nil {
				return err
			}
			record.Priority = &p
		}

		if rc.Type == "NS" && (name == "@" || name == "") {
			name = "*"
		}

		record.Name = &name
		record.Type = &rc.Type
		record.Content = &target
		if rc.TTL != 0 {
			ttl := int64(rc.TTL)
			record.TTL = &ttl
		}

		return c.client.UpdateDNSDomainRecord(
			context.Background(),
			c.apiZone,
			domainID,
			record,
		)
	}
}

func (c *exoscaleProvider) findDomainByName(name string) (*egoscale.DNSDomain, error) {
	domains, err := c.client.ListDNSDomains(context.Background(), c.apiZone)
	if err != nil {
		return nil, err
	}

	for _, domain := range domains {
		if domain.UnicodeName != nil && domain.ID != nil && *domain.UnicodeName == name {
			return &domain, nil
		}
	}

	return nil, ErrDomainNotFound
}

func defaultNSSUffix(defNS string) bool {
	return (strings.HasSuffix(defNS, ".exoscale.io.") ||
		strings.HasSuffix(defNS, ".exoscale.com.") ||
		strings.HasSuffix(defNS, ".exoscale.ch.") ||
		strings.HasSuffix(defNS, ".exoscale.net."))
}

// remove all non-exoscale NS records from our desired state.
// if any are found, print a warning
func removeOtherNS(dc *models.DomainConfig) {
	newList := make([]*models.RecordConfig, 0, len(dc.Records))
	for _, rec := range dc.Records {
		if rec.Type == "NS" {
			// apex NS inside exoscale are expected.
			if rec.GetLabelFQDN() == dc.Name && defaultNSSUffix(rec.GetTargetField()) {
				continue
			}
			printer.Printf("Warning: exoscale.com(.io, .ch, .net) does not allow NS records to be modified. %s will not be added.\n", rec.GetTargetField())
			continue
		}
		newList = append(newList, rec)
	}
	dc.Records = newList
}
