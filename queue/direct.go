package queue

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"github.com/mjl-/adns"
	"github.com/mjl-/bstore"

	"github.com/mjl-/mox/dns"
	"github.com/mjl-/mox/dsn"
	"github.com/mjl-/mox/mlog"
	"github.com/mjl-/mox/mox-"
	"github.com/mjl-/mox/mtasts"
	"github.com/mjl-/mox/mtastsdb"
	"github.com/mjl-/mox/smtp"
	"github.com/mjl-/mox/smtpclient"
	"github.com/mjl-/mox/store"
)

var (
	metricDestinations = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "mox_queue_destinations_total",
			Help: "Total destination (e.g. MX) lookups for delivery attempts, including those in mox_smtpclient_destinations_authentic_total.",
		},
	)
	metricDestinationsAuthentic = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "mox_queue_destinations_authentic_total",
			Help: "Destination (e.g. MX) lookups for delivery attempts authenticated with DNSSEC so they are candidates for DANE verification.",
		},
	)
	metricDestinationDANERequired = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "mox_queue_destination_dane_required_total",
			Help: "Total number of connections to hosts with valid TLSA records making DANE required.",
		},
	)
	metricDestinationDANESTARTTLSUnverified = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "mox_queue_destination_dane_starttlsunverified_total",
			Help: "Total number of connections with required DANE where all TLSA records were unusable.",
		},
	)
	metricDestinationDANEGatherTLSAErrors = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "mox_queue_destination_dane_gathertlsa_errors_total",
			Help: "Total number of connections where looking up TLSA records resulted in an error.",
		},
	)
	// todo: recognize when "tls-required-no" message header caused a non-verifying certificate to be overridden. requires doing our own certificate validation after having set tls.Config.InsecureSkipVerify due to tls-required-no.
	metricTLSRequiredNoIgnored = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "mox_queue_tlsrequiredno_ignored_total",
			Help: "Delivery attempts with TLS policy findings ignored due to message with TLS-Required: No header. Does not cover case where TLS certificate cannot be PKIX-verified.",
		},
		[]string{
			"ignored", // mtastspolicy (error getting policy), mtastsmx (mx host not allowed in policy), badtls (error negotiating tls), badtlsa (error fetching dane tlsa records)
		},
	)
	metricRequireTLSUnsupported = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "mox_queue_requiretls_unsupported_total",
			Help: "Delivery attempts that failed due to message with REQUIRETLS.",
		},
		[]string{
			"reason", // nopolicy (no mta-sts and no dane), norequiretls (smtp server does not support requiretls)
		},
	)
	metricPlaintextFallback = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "mox_queue_plaintext_fallback_total",
			Help: "Delivery attempts with fallback to plain text delivery.",
		},
	)
)

// todo: rename function, perhaps put some of the params in a delivery struct so we don't pass all the params all the time?
func fail(qlog *mlog.Log, m Msg, backoff time.Duration, permanent bool, remoteMTA dsn.NameIP, secodeOpt, errmsg string) {
	// todo future: when we implement relaying, we should be able to send DSNs to non-local users. and possibly specify a null mailfrom. ../rfc/5321:1503
	// todo future: when we implement relaying, and a dsn cannot be delivered, and requiretls was active, we cannot drop the message. instead deliver to local postmaster? though ../rfc/8689:383 may intend to say the dsn should be delivered without requiretls?
	// todo future: when we implement smtp dsn extension, parameter RET=FULL must be disregarded for messages with REQUIRETLS. ../rfc/8689:379

	if permanent || m.MaxAttempts == 0 && m.Attempts >= 8 || m.MaxAttempts > 0 && m.Attempts >= m.MaxAttempts {
		qlog.Errorx("permanent failure delivering from queue", errors.New(errmsg))
		deliverDSNFailure(qlog, m, remoteMTA, secodeOpt, errmsg)

		if err := queueDelete(context.Background(), m.ID); err != nil {
			qlog.Errorx("deleting message from queue after permanent failure", err)
		}
		return
	}

	qup := bstore.QueryDB[Msg](context.Background(), DB)
	qup.FilterID(m.ID)
	if _, err := qup.UpdateNonzero(Msg{LastError: errmsg, DialedIPs: m.DialedIPs}); err != nil {
		qlog.Errorx("storing delivery error", err, mlog.Field("deliveryerror", errmsg))
	}

	if m.Attempts == 5 {
		// We've attempted deliveries at these intervals: 0, 7.5m, 15m, 30m, 1h, 2u.
		// Let sender know delivery is delayed.
		qlog.Errorx("temporary failure delivering from queue, sending delayed dsn", errors.New(errmsg), mlog.Field("backoff", backoff))

		retryUntil := m.LastAttempt.Add((4 + 8 + 16) * time.Hour)
		deliverDSNDelay(qlog, m, remoteMTA, secodeOpt, errmsg, retryUntil)
	} else {
		qlog.Errorx("temporary failure delivering from queue", errors.New(errmsg), mlog.Field("backoff", backoff), mlog.Field("nextattempt", m.NextAttempt))
	}
}

// Delivery by directly dialing (MX) hosts for destination domain of message.
func deliverDirect(cid int64, qlog *mlog.Log, resolver dns.Resolver, dialer smtpclient.Dialer, ourHostname dns.Domain, transportName string, m Msg, backoff time.Duration) {
	// High-level approach:
	// - Resolve domain to deliver to (CNAME), and determine hosts to try to deliver to (MX)
	// - Get MTA-STS policy for domain (optional). If present, only deliver to its
	//   allowlisted hosts and verify TLS against CA pool.
	// - For each host, attempt delivery. If the attempt results in a permanent failure
	//   (as claimed by remote with a 5xx SMTP response, or perhaps decided by us), the
	//   attempt can be aborted. Other errors are often temporary and may result in later
	//   successful delivery. But hopefully the delivery just succeeds. For each host:
	//   - If there is an MTA-STS policy, we only connect to allow-listed hosts.
	//   - We try to lookup DANE records (optional) and verify them if present.
	//   - If RequireTLS is true, we only deliver if the remote SMTP server implements it.
	//   - If RequireTLS is false, we'll fall back to regular delivery attempts without
	//     TLS verification and possibly without TLS at all, ignoring recipient domain/host
	//     MTA-STS and DANE policies.

	// Resolve domain and hosts to attempt delivery to.
	// These next-hop names are often the name under which we find MX records. The
	// expanded name is different from the original if the original was a CNAME,
	// possibly a chain. If there are no MX records, it can be an IP or the host
	// directly.
	origNextHop := m.RecipientDomain.Domain
	ctx := context.WithValue(mox.Context, mlog.CidKey, cid)
	haveMX, origNextHopAuthentic, expandedNextHopAuthentic, expandedNextHop, hosts, permanent, err := smtpclient.GatherDestinations(ctx, qlog, resolver, m.RecipientDomain)
	if err != nil {
		fail(qlog, m, backoff, permanent, dsn.NameIP{}, "", err.Error())
		return
	}

	// Check for MTA-STS policy and enforce it if needed.
	// We must check at the original next-hop, i.e. recipient domain, not following any
	// CNAMEs. If we were to follow CNAMEs and ask for MTA-STS at that domain, it
	// would only take a single CNAME DNS response to direct us to an unrelated domain.
	var policy *mtasts.Policy
	if !origNextHop.IsZero() {
		cidctx := context.WithValue(mox.Shutdown, mlog.CidKey, cid)
		policy, _, err = mtastsdb.Get(cidctx, resolver, origNextHop)
		if err != nil {
			if m.RequireTLS != nil && !*m.RequireTLS {
				qlog.Infox("mtasts lookup temporary error, continuing due to tls-required-no message header", err, mlog.Field("domain", origNextHop))
				metricTLSRequiredNoIgnored.WithLabelValues("mtastspolicy").Inc()
			} else {
				qlog.Infox("mtasts lookup temporary error, aborting delivery attempt", err, mlog.Field("domain", origNextHop))
				fail(qlog, m, backoff, false, dsn.NameIP{}, "", err.Error())
				return
			}
		}
		// note: policy can be nil, if a domain does not implement MTA-STS or it's the
		// first time we fetch the policy and if we encountered an error.
	}

	// We try delivery to each host until we have success or a permanent failure. So
	// for transient errors, we'll try the next host. For MX records pointing to a
	// dual stack host, we turn a permanent failure due to policy on the first delivery
	// attempt into a temporary failure and make sure to try the other address family
	// the next attempt. This should reduce issues due to one of our IPs being on a
	// block list. We won't try multiple IPs of the same address family. Surprisingly,
	// RFC 5321 does not specify a clear algorithm, but common practice is probably
	// ../rfc/3974:268.
	var remoteMTA dsn.NameIP
	var secodeOpt, errmsg string
	permanent = false
	nmissingRequireTLS := 0
	// todo: should make distinction between host permanently not accepting the message, and the message not being deliverable permanently. e.g. a mx host may have a size limit, or not accept 8bitmime, while another host in the list does accept the message. same for smtputf8, ../rfc/6531:555
	for _, h := range hosts {
		// ../rfc/8461:913
		if policy != nil && !policy.Matches(h.Domain) {
			var policyHosts []string
			for _, mx := range policy.MX {
				policyHosts = append(policyHosts, mx.LogString())
			}
			if policy.Mode == mtasts.ModeEnforce {
				if m.RequireTLS != nil && !*m.RequireTLS {
					qlog.Info("mx host does not match mta-sts policy in mode enforce, ignoring due to tls-required-no message header", mlog.Field("host", h.Domain), mlog.Field("policyhosts", policyHosts))
					metricTLSRequiredNoIgnored.WithLabelValues("mtastsmx").Inc()
				} else {
					errmsg = fmt.Sprintf("mx host %s does not match enforced mta-sts policy with hosts %s", h.Domain, strings.Join(policyHosts, ","))
					qlog.Error("mx host does not match mta-sts policy in mode enforce, skipping", mlog.Field("host", h.Domain), mlog.Field("policyhosts", policyHosts))
					continue
				}
			} else {
				qlog.Error("mx host does not match mta-sts policy, but it is not enforced, continuing", mlog.Field("host", h.Domain), mlog.Field("policyhosts", policyHosts))
			}
		}

		qlog.Info("delivering to remote", mlog.Field("remote", h), mlog.Field("queuecid", cid))
		cid := mox.Cid()
		nqlog := qlog.WithCid(cid)
		var remoteIP net.IP

		tlsMode := smtpclient.TLSOpportunistic
		if policy != nil && policy.Mode == mtasts.ModeEnforce {
			tlsMode = smtpclient.TLSStrictStartTLS
		}

		// Try to deliver to host. We can get various errors back. Like permanent failure
		// response codes, TCP, DNSSEC, TLS (opportunistic, i.e. optional with fallback to
		// without), etc. It's a balancing act to handle these situations correctly. We
		// don't want to bounce unnecessarily. But also not keep trying if there is no
		// chance of success.

		// Set if there TLSA records were found. Means TLS is required for this host,
		// usually with verification of the certificate.
		var daneRequired bool

		var badTLS, ok bool
		enforceMTASTS := policy != nil && policy.Mode == mtasts.ModeEnforce
		permanent, daneRequired, badTLS, secodeOpt, remoteIP, errmsg, ok = deliverHost(nqlog, resolver, dialer, cid, ourHostname, transportName, h, enforceMTASTS, haveMX, origNextHopAuthentic, origNextHop, expandedNextHopAuthentic, expandedNextHop, &m, tlsMode)

		// If we had a TLS-related failure when doing TLS, and we don't have a requirement
		// for MTA-STS/DANE, we try again without TLS. This could be an old server that
		// only does ancient TLS versions, or has a misconfiguration. Note that
		// opportunistic TLS does not do regular certificate verification, so that can't be
		// the problem.
		// We don't fall back to plain text for DMARC reports. ../rfc/7489:1768 ../rfc/7489:2683
		if !ok && badTLS && (!enforceMTASTS && tlsMode == smtpclient.TLSOpportunistic && !daneRequired && !m.IsDMARCReport || m.RequireTLS != nil && !*m.RequireTLS) {
			metricPlaintextFallback.Inc()
			if m.RequireTLS != nil && !*m.RequireTLS {
				metricTLSRequiredNoIgnored.WithLabelValues("badtls").Inc()
			}

			// In case of failure with opportunistic TLS, try again without TLS. ../rfc/7435:459
			// todo future: add a configuration option to not fall back?
			nqlog.Info("connecting again for delivery attempt without tls", mlog.Field("enforcemtasts", enforceMTASTS), mlog.Field("danerequired", daneRequired), mlog.Field("requiretls", m.RequireTLS))
			tlsMode = smtpclient.TLSSkip
			permanent, _, _, secodeOpt, remoteIP, errmsg, ok = deliverHost(nqlog, resolver, dialer, cid, ourHostname, transportName, h, enforceMTASTS, haveMX, origNextHopAuthentic, origNextHop, expandedNextHopAuthentic, expandedNextHop, &m, tlsMode)
		}

		if ok {
			nqlog.Info("delivered from queue")
			if err := queueDelete(context.Background(), m.ID); err != nil {
				nqlog.Errorx("deleting message from queue after delivery", err)
			}
			return
		}
		remoteMTA = dsn.NameIP{Name: h.XString(false), IP: remoteIP}
		if permanent {
			break
		}
		if secodeOpt == smtp.SePol7MissingReqTLS {
			nmissingRequireTLS++
		}
	}

	// In theory, we could make a failure permanent if we didn't find any mx host
	// matching the mta-sts policy AND the policy is fresh AND all DNS records leading
	// to the MX targets (including CNAME) have a TTL that is beyond the latest
	// possible delivery attempt. Until that time, configuration problems can be
	// corrected through DNS or policy update. Not sure if worth it in practice, there
	// is a good chance the MX records can still change, at least on initial delivery
	// failures.
	// todo: possibly detect that future deliveries will fail due to long ttl's of cached records that are preventing delivery.

	// If we failed due to requiretls not being satisfied, make the delivery permanent.
	// It is unlikely the recipient domain will implement requiretls during our retry
	// period. Best to let the sender know immediately.
	if !permanent && nmissingRequireTLS > 0 && nmissingRequireTLS == len(hosts) {
		qlog.Info("marking delivery as permanently failed because recipient domain does not implement requiretls")
		permanent = true
	}

	fail(qlog, m, backoff, permanent, remoteMTA, secodeOpt, errmsg)
}

// deliverHost attempts to deliver m to host. Depending on tlsMode, we'll do
// required TLS with WebPKI verification (with MTA-STS), opportunistic DANE TLS
// (opportunistic TLS), non-verifying TLS (opportunistic TLS) or skip TLS
// altogether due to previous TLS errors.
//
// deliverHost updates m.DialedIPs, which must be saved in case of failure to
// deliver.
//
// With TLS-Required no header, we ignore verification failures and continue
// delivering.
//
// The haveMX and next-hop-authentic fields are used to determine if DANE is
// applicable. The next-hop fields themselves are used to determine valid names
// during DANE TLS certificate verification.
func deliverHost(log *mlog.Log, resolver dns.Resolver, dialer smtpclient.Dialer, cid int64, ourHostname dns.Domain, transportName string, host dns.IPDomain, enforceMTASTS, haveMX, origNextHopAuthentic bool, origNextHop dns.Domain, expandedNextHopAuthentic bool, expandedNextHop dns.Domain, m *Msg, tlsMode smtpclient.TLSMode) (permanent, daneRequired, badTLS bool, secodeOpt string, remoteIP net.IP, errmsg string, ok bool) {
	// About attempting delivery to multiple addresses of a host: ../rfc/5321:3898

	start := time.Now()
	var deliveryResult string
	defer func() {
		metricDelivery.WithLabelValues(fmt.Sprintf("%d", m.Attempts), transportName, string(tlsMode), deliveryResult).Observe(float64(time.Since(start)) / float64(time.Second))
		log.Debug("queue deliverhost result",
			mlog.Field("host", host),
			mlog.Field("attempt", m.Attempts),
			mlog.Field("tlsmode", tlsMode),
			mlog.Field("permanent", permanent),
			mlog.Field("badtls", badTLS),
			mlog.Field("secodeopt", secodeOpt),
			mlog.Field("errmsg", errmsg),
			mlog.Field("ok", ok),
			mlog.Field("duration", time.Since(start)))
	}()

	// Open message to deliver.
	f, err := os.Open(m.MessagePath())
	if err != nil {
		return false, false, false, "", nil, fmt.Sprintf("open message file: %s", err), false
	}
	msgr := store.FileMsgReader(m.MsgPrefix, f)
	defer func() {
		err := msgr.Close()
		log.Check(err, "closing message after delivery attempt")
	}()

	cidctx := context.WithValue(mox.Context, mlog.CidKey, cid)
	ctx, cancel := context.WithTimeout(cidctx, 30*time.Second)
	defer cancel()

	// We must lookup the IPs for the host name before checking DANE TLSA records. And
	// only check TLSA records for secure responses. This prevents problems with old
	// name servers returning an error for TLSA requests or letting it timeout (not
	// sending a response). ../rfc/7672:879
	var daneRecords []adns.TLSA
	var tlsRemoteHostnames []dns.Domain
	if host.IsDomain() {
		tlsRemoteHostnames = []dns.Domain{host.Domain}
	}
	if m.DialedIPs == nil {
		m.DialedIPs = map[string][]net.IP{}
	}
	metricDestinations.Inc()
	authentic, expandedAuthentic, expandedHost, ips, dualstack, err := smtpclient.GatherIPs(ctx, log, resolver, host, m.DialedIPs)
	if err == nil && authentic && origNextHopAuthentic && (!haveMX || expandedNextHopAuthentic) && host.IsDomain() {
		metricDestinationsAuthentic.Inc()

		switch tlsMode {
		case smtpclient.TLSSkip:
			// No TLS, so clearly no DANE. This can happen if we've dialed TLS before but a TLS
			// connection couldn't be established.
		case smtpclient.TLSUnverifiedStartTLS:
			// Fallback mode for DANE without usable records, so skip DANE.
			// We shouldn't be able to get here, but no harm handling it.
		default:
			// Look for TLSA records in either the expandedHost, or otherwise the original
			// host. ../rfc/7672:912
			var tlsaBaseDomain dns.Domain
			daneRequired, daneRecords, tlsaBaseDomain, err = smtpclient.GatherTLSA(ctx, log, resolver, host.Domain, expandedNextHopAuthentic && expandedAuthentic, expandedHost)
			if daneRequired {
				metricDestinationDANERequired.Inc()
			}
			if err != nil {
				metricDestinationDANEGatherTLSAErrors.Inc()
			}
			if err == nil && daneRequired {
				tlsMode = smtpclient.TLSStrictStartTLS
				if len(daneRecords) == 0 {
					// If there are no usable DANE records, we still have to use TLS, but without
					// verifying its certificate. At least when there is no MTA-STS. Why? Perhaps to
					// prevent ossification? The SMTP TLSA specification has different behaviour than
					// the generic TLSA. "Usable" means different things in different places.
					// ../rfc/7672:718 ../rfc/6698:1845 ../rfc/6698:660
					if !enforceMTASTS {
						tlsMode = smtpclient.TLSUnverifiedStartTLS
						log.Debug("no usable dane records, not verifying dane records, but doing required non-verifying opportunistic tls")
						metricDestinationDANESTARTTLSUnverified.Inc()
					}
					daneRecords = nil
				} else {
					// Based on CNAMEs followed and DNSSEC-secure status, we must allow up to 4 host
					// names.
					tlsRemoteHostnames = smtpclient.GatherTLSANames(haveMX, expandedNextHopAuthentic, expandedAuthentic, origNextHop, expandedNextHop, host.Domain, tlsaBaseDomain)
					log.Debug("delivery with required starttls with dane verification", mlog.Field("allowedtlshostnames", tlsRemoteHostnames))
				}
			} else if !daneRequired {
				log.Debugx("not doing opportunistic dane after gathering tlsa records", err)
				err = nil
			} else if err != nil && m.RequireTLS != nil && !*m.RequireTLS {
				log.Debugx("error gathering dane tlsa records with dane required, but continuing without validation due to tls-required-no message header", err)
				daneRecords = nil
				err = nil
				metricTLSRequiredNoIgnored.WithLabelValues("badtlsa").Inc()
			}
			// else, err is propagated below.
		}
	} else {
		log.Debugx("not attempting verification with dane", err, mlog.Field("authentic", authentic), mlog.Field("expandedauthentic", expandedAuthentic))
	}

	// todo: for requiretls, should an MTA-STS policy in mode testing be treated as good enough for requiretls? let's be strict and assume not.
	// todo: ../rfc/8689:276 seems to specify stricter requirements on name in certificate than DANE (which allows original recipient domain name and cname-expanded name, and hints at following CNAME for MX targets as well, allowing both their original and expanded names too). perhaps the intent was just to say the name must be validated according to the relevant specifications?
	// todo: for requiretls, should we allow no usable dane records with requiretls? dane allows it, but doesn't seem in spirit of requiretls, so not allowing it.
	if err == nil && m.RequireTLS != nil && *m.RequireTLS && !(daneRequired && len(daneRecords) > 0) && !enforceMTASTS {
		log.Info("verified tls is required, but destination has no usable dane records and no mta-sts policy, canceling delivery attempt to host")
		metricRequireTLSUnsupported.WithLabelValues("nopolicy").Inc()
		// Resond with proper enhanced status code. ../rfc/8689:301
		return false, daneRequired, false, smtp.SePol7MissingReqTLS, remoteIP, "missing required tls verification mechanism", false
	}

	// Dial the remote host given the IPs if no error yet.
	var conn net.Conn
	if err == nil {
		if m.DialedIPs == nil {
			m.DialedIPs = map[string][]net.IP{}
		}
		conn, remoteIP, err = smtpclient.Dial(ctx, log, dialer, host, ips, 25, m.DialedIPs)
	}
	cancel()

	// Set error for metrics.
	var result string
	switch {
	case err == nil:
		result = "ok"
	case errors.Is(err, os.ErrDeadlineExceeded), errors.Is(err, context.DeadlineExceeded):
		result = "timeout"
	case errors.Is(err, context.Canceled):
		result = "canceled"
	default:
		result = "error"
	}
	metricConnection.WithLabelValues(result).Inc()
	if err != nil {
		log.Debugx("connecting to remote smtp", err, mlog.Field("host", host))
		return false, daneRequired, false, "", remoteIP, fmt.Sprintf("dialing smtp server: %v", err), false
	}

	var mailFrom string
	if m.SenderLocalpart != "" || !m.SenderDomain.IsZero() {
		mailFrom = m.Sender().XString(m.SMTPUTF8)
	}
	rcptTo := m.Recipient().XString(m.SMTPUTF8)

	// todo future: get closer to timeouts specified in rfc? ../rfc/5321:3610
	log = log.Fields(mlog.Field("remoteip", remoteIP))
	ctx, cancel = context.WithTimeout(cidctx, 30*time.Minute)
	defer cancel()
	mox.Connections.Register(conn, "smtpclient", "queue")

	// Initialize SMTP session, sending EHLO/HELO and STARTTLS with specified tls mode.
	var firstHost dns.Domain
	var moreHosts []dns.Domain
	if len(tlsRemoteHostnames) > 0 {
		// For use with DANE-TA.
		firstHost = tlsRemoteHostnames[0]
		moreHosts = tlsRemoteHostnames[1:]
	}
	var verifiedRecord adns.TLSA
	if m.RequireTLS != nil && !*m.RequireTLS && tlsMode != smtpclient.TLSSkip {
		tlsMode = smtpclient.TLSUnverifiedStartTLS
	}
	sc, err := smtpclient.New(ctx, log, conn, tlsMode, ourHostname, firstHost, nil, daneRecords, moreHosts, &verifiedRecord)
	defer func() {
		if sc == nil {
			conn.Close()
		} else {
			sc.Close()
		}
		mox.Connections.Unregister(conn)
	}()
	if err == nil && m.SenderAccount != "" {
		// Remember the STARTTLS and REQUIRETLS support for this recipient domain.
		// It is used in the webmail client, to show the recipient domain security mechanisms.
		// We always save only the last connection we actually encountered. There may be
		// multiple MX hosts, perhaps only some support STARTTLS and REQUIRETLS. We may not
		// be accurate for the whole domain, but we're only storing a hint.
		rdt := store.RecipientDomainTLS{
			Domain:     m.RecipientDomain.Domain.Name(),
			STARTTLS:   sc.TLSEnabled(),
			RequireTLS: sc.SupportsRequireTLS(),
		}
		if err = updateRecipientDomainTLS(ctx, m.SenderAccount, rdt); err != nil {
			err = fmt.Errorf("storing recipient domain tls status: %w", err)
		}
	}
	if err == nil {
		// SMTP session is ready. Finally try to actually deliver.
		has8bit := m.Has8bit
		smtputf8 := m.SMTPUTF8
		var msg io.Reader = msgr
		size := m.Size
		if m.DSNUTF8 != nil && sc.Supports8BITMIME() && sc.SupportsSMTPUTF8() {
			has8bit = true
			smtputf8 = true
			size = int64(len(m.DSNUTF8))
			msg = bytes.NewReader(m.DSNUTF8)
		}
		err = sc.Deliver(ctx, mailFrom, rcptTo, size, msg, has8bit, smtputf8, m.RequireTLS != nil && *m.RequireTLS)
	}
	if err != nil {
		log.Infox("delivery failed", err)
	}
	var cerr smtpclient.Error
	switch {
	case err == nil:
		deliveryResult = "ok"
	case errors.Is(err, os.ErrDeadlineExceeded), errors.Is(err, context.DeadlineExceeded):
		deliveryResult = "timeout"
	case errors.Is(err, context.Canceled):
		deliveryResult = "canceled"
	case errors.As(err, &cerr):
		deliveryResult = "temperror"
		if cerr.Permanent {
			deliveryResult = "permerror"
		}
	default:
		deliveryResult = "error"
	}
	if err == nil {
		return false, daneRequired, false, "", remoteIP, "", true
	} else if cerr, ok := err.(smtpclient.Error); ok {
		// If we are being rejected due to policy reasons on the first
		// attempt and remote has both IPv4 and IPv6, we'll give it
		// another try. Our first IP may be in a block list, the address for
		// the other family perhaps is not.
		permanent := cerr.Permanent
		if permanent && m.Attempts == 1 && dualstack && strings.HasPrefix(cerr.Secode, "7.") {
			permanent = false
		}
		// If server does not implement requiretls, respond with that code. ../rfc/8689:301
		secode := cerr.Secode
		if errors.Is(cerr.Err, smtpclient.ErrRequireTLSUnsupported) {
			secode = smtp.SePol7MissingReqTLS
			metricRequireTLSUnsupported.WithLabelValues("norequiretls").Inc()
		}
		return permanent, daneRequired, errors.Is(cerr, smtpclient.ErrTLS), secode, remoteIP, cerr.Error(), false
	} else {
		return false, daneRequired, errors.Is(cerr, smtpclient.ErrTLS), "", remoteIP, err.Error(), false
	}
}

// Update (overwite) last known starttls/requiretls support for recipient domain.
func updateRecipientDomainTLS(ctx context.Context, senderAccount string, rdt store.RecipientDomainTLS) error {
	acc, err := store.OpenAccount(senderAccount)
	if err != nil {
		return fmt.Errorf("open account: %w", err)
	}
	err = acc.DB.Write(ctx, func(tx *bstore.Tx) error {
		// First delete any existing record.
		if err := tx.Delete(&store.RecipientDomainTLS{Domain: rdt.Domain}); err != nil && err != bstore.ErrAbsent {
			return fmt.Errorf("removing previous recipient domain tls status: %w", err)
		}
		// Insert new record.
		return tx.Insert(&rdt)
	})
	if err != nil {
		return fmt.Errorf("adding recipient domain tls status to account database: %w", err)
	}
	return nil
}
