// Package disputes wraps Stripe's chargeback / dispute API. Apps
// receive dispute lifecycle events via webhooks.Handlers.OnDispute*
// (typed since slice 48); this package gives them the *submission*
// side so they can respond to the dispute with evidence rather than
// fight chargebacks from the Stripe Dashboard.
//
// Dispute timeline:
//
//	customer disputes → charge.dispute.created webhook → app has ~7-21
//	days (network-dependent) to submit evidence → charge.dispute.closed
//	(won/lost) → if lost, funds_withdrawn webhook fires.
//
// Apps that don't fight disputes can call Close to acknowledge loss
// (Stripe credits the dispute fee back vs. failing to respond).
package disputes

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/bds421/rho-stripe/stripeapi"
	stripe "github.com/stripe/stripe-go/v82"
)

// Operations is the entry point. Construct via New, then call via
// conn.Disputes.{Submit,Close,Get,List}.
type Operations struct {
	sc *stripe.Client
}

// New returns an Operations wrapping the supplied Stripe client.
// Panics if sc is nil — matches lib-wide constructor convention.
func New(sc *stripe.Client) *Operations {
	if sc == nil {
		panic("disputes.New: StripeClient is required")
	}
	return &Operations{sc: sc}
}

// Dispute is the lib's stable projection of stripe.Dispute.
type Dispute struct {
	StripeID           string
	ChargeID           string
	PaymentIntentID    string
	Amount             int64
	Currency           string
	Reason             string // "credit_not_processed", "fraudulent", "product_not_received", ...
	Status             string // "needs_response", "warning_needs_response", "won", "lost", ...
	EvidenceDueBy      *time.Time
	IsChargeRefundable bool
	HasEvidence        bool
	CreatedAt          time.Time
}

// Evidence collects the fields Stripe accepts for dispute response.
// Every field is optional; fill what's relevant to the dispute reason
// (a "product_not_received" needs shipping evidence; "fraudulent"
// needs customer-verified-purchase evidence).
//
// The combined character count of string fields is limited to
// 150,000 by Stripe. File-id fields refer to objects uploaded
// separately via Stripe's files API.
type Evidence struct {
	// Customer-identifying
	CustomerName       string
	CustomerEmail      string
	CustomerPurchaseIP string
	BillingAddress     string

	// Product / service evidence
	ProductDescription   string
	ServiceDate          string
	ServiceDocumentation string // file id

	// Shipping evidence (for physical-product disputes)
	ShippingAddress        string
	ShippingDate           string
	ShippingCarrier        string
	ShippingTrackingNumber string
	ShippingDocumentation  string // file id

	// Communication / agreement
	CustomerSignature        string // file id
	CustomerCommunication    string // file id
	Receipt                  string // file id
	RefundPolicy             string // file id
	RefundPolicyDisclosure   string
	RefundRefusalExplanation string

	// Subscription-specific
	CancellationPolicy           string // file id
	CancellationPolicyDisclosure string
	CancellationRebuttal         string

	// Duplicate-charge evidence
	DuplicateChargeID            string
	DuplicateChargeExplanation   string
	DuplicateChargeDocumentation string // file id

	// Digital product / access logs
	AccessActivityLog string

	// Catch-all
	UncategorizedFile string // file id
	UncategorizedText string

	// EnhancedEvidence carries Visa Compelling Evidence 3.0 +
	// Visa Compliance evidence programs. Critical for dispute-win
	// rates on Visa chargebacks — without it, Visa applies
	// the "no compelling evidence" presumption against the merchant.
	// Nil when not applicable / not collected.
	EnhancedEvidence *EnhancedEvidence
}

// EnhancedEvidence carries dispute-win-rate-boosting evidence Stripe
// passes to Visa's "Compelling Evidence 3.0" + Compliance programs.
type EnhancedEvidence struct {
	VisaCompellingEvidence3 *VisaCE3
	VisaCompliance          *VisaCompliance
}

// VisaCE3 holds Compelling Evidence 3.0 fields. Stripe documents the
// schema at https://stripe.com/docs/disputes/compelling-evidence-3-0.
type VisaCE3 struct {
	DisputedTransactionID       string
	PriorUndisputedTransactions []string // up to 5 PaymentIntent ids
}

// VisaCompliance holds compliance-program evidence (less common but
// reduces fee exposure on legitimately-charged-but-disputed cards).
type VisaCompliance struct {
	FeeAcknowledged bool
}

// hashBytes returns a stable byte string for content-hash derivation.
func (e *EnhancedEvidence) hashBytes() string {
	var s string
	if e.VisaCompellingEvidence3 != nil {
		s += "ce3:" + e.VisaCompellingEvidence3.DisputedTransactionID + ":"
		for _, t := range e.VisaCompellingEvidence3.PriorUndisputedTransactions {
			s += t + ","
		}
	}
	if e.VisaCompliance != nil {
		s += "comp:"
		if e.VisaCompliance.FeeAcknowledged {
			s += "1"
		} else {
			s += "0"
		}
	}
	return s
}

// SubmitEvidence submits evidence to Stripe. Once submitted, Stripe
// queues the dispute for the network's review (typically 60-75 days).
// Submitting evidence sets the dispute to "under_review" status.
//
// Re-submission (within the response window) is allowed; the full
// evidence object is replaced each call. The idempotency key includes
// a content hash of the Evidence struct, so re-submitting *the same*
// evidence dedups but a re-submission *with new content* gets a fresh
// key and actually replaces the prior submission.
func (o *Operations) SubmitEvidence(ctx context.Context, disputeID string, evidence Evidence) (*Dispute, error) {
	if disputeID == "" {
		return nil, errors.New("disputes.SubmitEvidence: disputeID is required")
	}
	params := &stripe.DisputeUpdateParams{
		Evidence: buildEvidenceParams(evidence),
	}
	stripeapi.ApplyIdempotencyKey(params, ctx, "disputes.submit_evidence",
		disputeID, evidenceContentHash(evidence))
	updated, err := o.sc.V1Disputes.Update(ctx, disputeID, params)
	if err != nil {
		return nil, fmt.Errorf("disputes.SubmitEvidence: Disputes.Update(%s): %w", disputeID, err)
	}
	return projectDispute(updated), nil
}

// evidenceContentHash hashes every Evidence field via stripeapi.ContentHash
// so two distinct evidence bundles for the same dispute get distinct
// idempotency keys.
func evidenceContentHash(e Evidence) string {
	fields := map[string]string{
		"AccessActivityLog":            e.AccessActivityLog,
		"BillingAddress":               e.BillingAddress,
		"CancellationPolicy":           e.CancellationPolicy,
		"CancellationPolicyDisclosure": e.CancellationPolicyDisclosure,
		"CancellationRebuttal":         e.CancellationRebuttal,
		"CustomerCommunication":        e.CustomerCommunication,
		"CustomerEmail":                e.CustomerEmail,
		"CustomerName":                 e.CustomerName,
		"CustomerPurchaseIP":           e.CustomerPurchaseIP,
		"CustomerSignature":            e.CustomerSignature,
		"DuplicateChargeDocumentation": e.DuplicateChargeDocumentation,
		"DuplicateChargeExplanation":   e.DuplicateChargeExplanation,
		"DuplicateChargeID":            e.DuplicateChargeID,
		"ProductDescription":           e.ProductDescription,
		"Receipt":                      e.Receipt,
		"RefundPolicy":                 e.RefundPolicy,
		"RefundPolicyDisclosure":       e.RefundPolicyDisclosure,
		"RefundRefusalExplanation":     e.RefundRefusalExplanation,
		"ServiceDate":                  e.ServiceDate,
		"ServiceDocumentation":         e.ServiceDocumentation,
		"ShippingAddress":              e.ShippingAddress,
		"ShippingCarrier":              e.ShippingCarrier,
		"ShippingDate":                 e.ShippingDate,
		"ShippingDocumentation":        e.ShippingDocumentation,
		"ShippingTrackingNumber":       e.ShippingTrackingNumber,
		"UncategorizedFile":            e.UncategorizedFile,
		"UncategorizedText":            e.UncategorizedText,
	}
	if e.EnhancedEvidence != nil {
		fields["EnhancedEvidence"] = e.EnhancedEvidence.hashBytes()
	}
	return stripeapi.ContentHash("disputes.evidence", fields)
}

// Close acknowledges loss of the dispute. The status transitions from
// `needs_response` → `lost`. Irreversible. Apps that won't submit
// evidence should call Close to avoid the "did not respond" penalty
// Stripe assesses on networks like Mastercard.
func (o *Operations) Close(ctx context.Context, disputeID string) (*Dispute, error) {
	if disputeID == "" {
		return nil, errors.New("disputes.Close: disputeID is required")
	}
	params := &stripe.DisputeCloseParams{}
	stripeapi.ApplyIdempotencyKey(params, ctx, "disputes.close", disputeID)
	closed, err := o.sc.V1Disputes.Close(ctx, disputeID, params)
	if err != nil {
		return nil, fmt.Errorf("disputes.Close(%s): %w", disputeID, err)
	}
	return projectDispute(closed), nil
}

// Retrieve fetches a dispute by id.
func (o *Operations) Retrieve(ctx context.Context, disputeID string) (*Dispute, error) {
	if disputeID == "" {
		return nil, errors.New("disputes.Retrieve: disputeID is required")
	}
	got, err := o.sc.V1Disputes.Retrieve(ctx, disputeID, nil)
	if err != nil {
		return nil, fmt.Errorf("disputes.Retrieve(%s): %w", disputeID, err)
	}
	return projectDispute(got), nil
}

// ListInput controls filtering for List.
type ListInput struct {
	// ChargeID, when set, limits to disputes for that one charge.
	ChargeID string
	// PaymentIntentID, when set, limits to disputes for that PI.
	PaymentIntentID string
	// Limit caps the page size. Default 50, max 100.
	Limit int
}

// List returns every dispute matching the filter, newest first.
func (o *Operations) List(ctx context.Context, in ListInput) ([]*Dispute, error) {
	params := &stripe.DisputeListParams{}
	if in.ChargeID != "" {
		params.Charge = stripe.String(in.ChargeID)
	}
	if in.PaymentIntentID != "" {
		params.PaymentIntent = stripe.String(in.PaymentIntentID)
	}
	limit := in.Limit
	if limit <= 0 {
		limit = 50
	}
	params.Filters.AddFilter("limit", "", fmt.Sprintf("%d", limit))

	var out []*Dispute
	for d, err := range o.sc.V1Disputes.List(ctx, params) {
		if err != nil {
			return nil, fmt.Errorf("disputes.List: %w", err)
		}
		out = append(out, projectDispute(d))
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

func buildEvidenceParams(e Evidence) *stripe.DisputeUpdateEvidenceParams {
	p := &stripe.DisputeUpdateEvidenceParams{}
	setIf(&p.AccessActivityLog, e.AccessActivityLog)
	setIf(&p.BillingAddress, e.BillingAddress)
	setIf(&p.CancellationPolicy, e.CancellationPolicy)
	setIf(&p.CancellationPolicyDisclosure, e.CancellationPolicyDisclosure)
	setIf(&p.CancellationRebuttal, e.CancellationRebuttal)
	setIf(&p.CustomerCommunication, e.CustomerCommunication)
	setIf(&p.CustomerEmailAddress, e.CustomerEmail)
	setIf(&p.CustomerName, e.CustomerName)
	setIf(&p.CustomerPurchaseIP, e.CustomerPurchaseIP)
	setIf(&p.CustomerSignature, e.CustomerSignature)
	setIf(&p.DuplicateChargeDocumentation, e.DuplicateChargeDocumentation)
	setIf(&p.DuplicateChargeExplanation, e.DuplicateChargeExplanation)
	setIf(&p.DuplicateChargeID, e.DuplicateChargeID)
	setIf(&p.ProductDescription, e.ProductDescription)
	setIf(&p.Receipt, e.Receipt)
	setIf(&p.RefundPolicy, e.RefundPolicy)
	setIf(&p.RefundPolicyDisclosure, e.RefundPolicyDisclosure)
	setIf(&p.RefundRefusalExplanation, e.RefundRefusalExplanation)
	setIf(&p.ServiceDate, e.ServiceDate)
	setIf(&p.ServiceDocumentation, e.ServiceDocumentation)
	setIf(&p.ShippingAddress, e.ShippingAddress)
	setIf(&p.ShippingCarrier, e.ShippingCarrier)
	setIf(&p.ShippingDate, e.ShippingDate)
	setIf(&p.ShippingDocumentation, e.ShippingDocumentation)
	setIf(&p.ShippingTrackingNumber, e.ShippingTrackingNumber)
	setIf(&p.UncategorizedFile, e.UncategorizedFile)
	setIf(&p.UncategorizedText, e.UncategorizedText)
	if e.EnhancedEvidence != nil {
		p.EnhancedEvidence = buildEnhancedEvidenceParams(e.EnhancedEvidence)
	}
	return p
}

func buildEnhancedEvidenceParams(e *EnhancedEvidence) *stripe.DisputeUpdateEvidenceEnhancedEvidenceParams {
	out := &stripe.DisputeUpdateEvidenceEnhancedEvidenceParams{}
	if e.VisaCompellingEvidence3 != nil {
		ce3 := &stripe.DisputeUpdateEvidenceEnhancedEvidenceVisaCompellingEvidence3Params{
			DisputedTransaction: &stripe.DisputeUpdateEvidenceEnhancedEvidenceVisaCompellingEvidence3DisputedTransactionParams{},
		}
		setIf(&ce3.DisputedTransaction.CustomerAccountID, e.VisaCompellingEvidence3.DisputedTransactionID)
		out.VisaCompellingEvidence3 = ce3
	}
	if e.VisaCompliance != nil {
		out.VisaCompliance = &stripe.DisputeUpdateEvidenceEnhancedEvidenceVisaComplianceParams{
			FeeAcknowledged: stripe.Bool(e.VisaCompliance.FeeAcknowledged),
		}
	}
	return out
}

func setIf(dst **string, s string) {
	if s != "" {
		*dst = stripe.String(s)
	}
}

func projectDispute(d *stripe.Dispute) *Dispute {
	out := &Dispute{
		StripeID:           d.ID,
		Amount:             d.Amount,
		Currency:           string(d.Currency),
		Reason:             string(d.Reason),
		Status:             string(d.Status),
		IsChargeRefundable: d.IsChargeRefundable,
		CreatedAt:          time.Unix(d.Created, 0).UTC(),
	}
	if d.Charge != nil {
		out.ChargeID = d.Charge.ID
	}
	if d.PaymentIntent != nil {
		out.PaymentIntentID = d.PaymentIntent.ID
	}
	if d.EvidenceDetails != nil && d.EvidenceDetails.DueBy > 0 {
		t := time.Unix(d.EvidenceDetails.DueBy, 0).UTC()
		out.EvidenceDueBy = &t
		out.HasEvidence = d.EvidenceDetails.HasEvidence
	}
	return out
}
