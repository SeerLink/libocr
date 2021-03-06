package protocol

import (
	"context"
	"math/big"
	"sort"
	"time"

	"github.com/pkg/errors"
	"github.com/SeerLink/libocr/offchainreporting/internal/protocol/observation"
	"github.com/SeerLink/libocr/offchainreporting/internal/signature"
	"github.com/SeerLink/libocr/offchainreporting/types"
)

func (repgen *reportGenerationState) followerReportContext() ReportContext {
	return ReportContext{repgen.config.ConfigDigest, repgen.e, repgen.followerState.r}
}

///////////////////////////////////////////////////////////
// Report Generation Follower (Algorithm 2)
///////////////////////////////////////////////////////////

// messageObserveReq is called when the oracle receives an observe-req message
// from the current leadrepgen. It responds with a message to the leader
// containing a fresh observation, as long as the message comes from the
// designated leader, pertains to the current valid round/epoch. It sets up the
// follower state used to track which the protocol is at in view of this
// follower.
func (repgen *reportGenerationState) messageObserveReq(msg MessageObserveReq, sender types.OracleID) {
	dropPrefix := "messageObserveReq: dropping MessageObserveReq from "
	// Each of these guards get their own if statement, to ease test-coverage
	// verification
	if msg.Epoch != repgen.e {
		repgen.logger.Debug(dropPrefix+"wrong epoch",
			types.LogFields{"round": repgen.followerState.r, "msgEpoch": msg.Epoch},
		)
		return
	}
	if sender != repgen.l {
		// warn because someone *from this epoch* is trying to usurp the lead
		repgen.logger.Warn(dropPrefix+"non-leader",
			types.LogFields{"round": repgen.followerState.r, "sender": sender})
		return
	}
	if msg.Round <= repgen.followerState.r {
		// this can happen due to network delays, so it's only a debug output
		repgen.logger.Debug(dropPrefix+"earlier round",
			types.LogFields{"round": repgen.followerState.r, "msgRound": msg.Round})
		return
	}
	if int64(repgen.config.RMax)+1 < int64(msg.Round) {
		// This check prevents the leader from triggering the changeleader behavior
		// an arbitrary number of times (with round=RMax+2, RMax+3, ...) until
		// consensus on the next epoch has developed. Since advancing to the next
		// epoch involves broadcast network messages from all participants, a
		// malicious leader could otherwise potentially trigger a network flood.
		//
		// Warn because the leader should never send a round value this high
		repgen.logger.Warn(dropPrefix+"out of bounds round",
			types.LogFields{"round": repgen.followerState.r, "rMax": repgen.config.RMax, "msgRound": msg.Round})
		return
	}

	// msg.Round>0, because msg.Round>repgen.followerState.r, and the initial
	// value of repgen.followerState.r is zero. msg.Round<=repgen.config.RMax
	// thus ensures that at most rMax rounds are possible for the current leader.
	repgen.followerState.r = msg.Round

	if repgen.followerState.r > repgen.config.RMax {
		repgen.logger.Debug(
			"messageReportReq: leader sent MessageObserveReq past its expiration "+
				"round. Time to change leader",
			types.LogFields{
				"round":        repgen.followerState.r,
				"messageRound": msg.Round,
				"roundMax":     repgen.config.RMax,
			})
		select {
		case repgen.chReportGenerationToPacemaker <- EventChangeLeader{}:
		case <-repgen.ctx.Done():
		}

		return
	}
	// A malicious leader could reset these values by sending an observeReq later
	// in the protocol, but they would only harm themselves, because that would
	// advance the follower's view of the current epoch's round, which only
	// reduces the number of rounds the current leader has left to report in
	// without influencing the transmitted report in any way. (A valid observeReq
	// after the report has been passed to the transmission machinery is expected,
	// and has no impact on the transmission process.)
	repgen.followerState.sentEcho = nil
	repgen.followerState.sentReport = false
	repgen.followerState.completedRound = false
	repgen.followerState.receivedEcho = make([]bool, repgen.config.N())

	repgen.telemetrySender.RoundStarted(
		repgen.config.ConfigDigest,
		repgen.e,
		repgen.followerState.r,
		repgen.l,
	)

	value := repgen.observeValue()
	if value.IsMissingValue() {
		// Failed to get data from API, nothing to be done...
		// No need to log because observeValue already does
		return
	}

	so, err := MakeSignedObservation(value, repgen.followerReportContext(), repgen.privateKeys.SignOffChain)
	if err != nil {
		repgen.logger.Error("messageObserveReq: could not make SignedObservation observation", types.LogFields{
			"round": repgen.followerState.r,
			"error": err,
		})
		return
	}

	if err := so.Verify(repgen.followerReportContext(), repgen.privateKeys.PublicKeyOffChain()); err != nil {
		repgen.logger.Error("MakeSignedObservation produced invalid signature:", types.LogFields{
			"round": repgen.followerState.r,
			"error": err,
		})
		return
	}

	repgen.logger.Debug("sent observation to leader", types.LogFields{
		"round":       repgen.followerState.r,
		"observation": value,
	})
	repgen.netSender.SendTo(MessageObserve{
		repgen.e,
		repgen.followerState.r,
		so,
	}, repgen.l)
}

// messageReportReq is called when an oracle receives a report-req message from
// the current leader. If the contained report validates, the oracle signs it
// and sends it back to the leader.
func (repgen *reportGenerationState) messageReportReq(msg MessageReportReq, sender types.OracleID) {
	// Each of these guards get their own if statement, to ease test-coverage
	// verification
	if repgen.e != msg.Epoch {
		repgen.logger.Debug("messageReportReq from wrong epoch", types.LogFields{
			"round":    repgen.followerState.r,
			"msgEpoch": msg.Epoch})
		return
	}
	if sender != repgen.l {
		// warn because someone *from this epoch* is trying to usurp the lead
		repgen.logger.Warn("messageReportReq from non-leader", types.LogFields{
			"round": repgen.followerState.r, "sender": sender})
		return
	}
	if repgen.followerState.r != msg.Round {
		// too low a round can happen due to network delays, too high if the local
		// oracle loses network connectivity. So this is only debug-level
		repgen.logger.Debug("messageReportReq from wrong round", types.LogFields{
			"round": repgen.followerState.r, "msgRound": msg.Round})
		return
	}
	if repgen.followerState.sentReport {
		repgen.logger.Warn("messageReportReq after report sent", types.LogFields{
			"round": repgen.followerState.r, "msgRound": msg.Round})
		return
	}
	if repgen.followerState.completedRound {
		repgen.logger.Warn("messageReportReq after round completed", types.LogFields{
			"round": repgen.followerState.r, "msgRound": msg.Round})
		return
	}
	err := repgen.verifyReportReq(msg)
	if err != nil {
		repgen.logger.Error("messageReportReq: could not validate report sent by leader", types.LogFields{
			"round": repgen.followerState.r,
			"error": err,
			"msg":   msg,
		})
		return
	}

	if repgen.shouldReport(msg.AttributedSignedObservations) {
		attributedValues := make([]AttributedObservation, len(msg.AttributedSignedObservations))
		for i, aso := range msg.AttributedSignedObservations {
			// Observation/Observer attribution is verified by checking signature in verifyReportReq
			attributedValues[i] = AttributedObservation{
				aso.SignedObservation.Observation,
				aso.Observer,
			}
		}

		report, err := MakeAttestedReportOne(
			attributedValues,
			repgen.followerReportContext(),
			repgen.privateKeys.SignOnChain,
		)
		if err != nil {
			// Can't really do much here except logging as much detail as possible to
			// aid reproduction, and praying it won't happen again
			repgen.logger.Error("messageReportReq: failed to sign report", types.LogFields{
				"round":  repgen.followerState.r,
				"error":  err,
				"id":     repgen.id,
				"report": report,
				"pubkey": repgen.privateKeys.PublicKeyAddressOnChain(),
			})
			return
		}

		{
			err := report.Verify(repgen.followerReportContext(), repgen.privateKeys.PublicKeyAddressOnChain())
			if err != nil {
				repgen.logger.Error("could not verify my own signature", types.LogFields{
					"round":  repgen.followerState.r,
					"error":  err,
					"id":     repgen.id,
					"report": report, // includes sig
					"pubkey": repgen.privateKeys.PublicKeyAddressOnChain()})
				return
			}
		}

		repgen.followerState.sentReport = true
		repgen.netSender.SendTo(
			MessageReport{
				repgen.e,
				repgen.followerState.r,
				report,
			},
			repgen.l,
		)
	} else {
		repgen.completeRound()
	}
}

// messageFinal is called when a "final" message is received for the local
// oracle process. If the report in the msg is valid, the oracle broadcasts it
// in a "final-echo" message.
func (repgen *reportGenerationState) messageFinal(
	msg MessageFinal, sender types.OracleID,
) {
	if msg.Epoch != repgen.e {
		repgen.logger.Debug("wrong epoch from MessageFinal", types.LogFields{
			"round": repgen.followerState.r, "msgEpoch": msg.Epoch, "sender": sender})
		return
	}
	if msg.Round != repgen.followerState.r {
		repgen.logger.Debug("wrong round from MessageFinal", types.LogFields{
			"round": repgen.followerState.r, "msgRound": msg.Round})
		return
	}
	if sender != repgen.l {
		repgen.logger.Warn("MessageFinal from non-leader", types.LogFields{
			"msgEpoch": msg.Epoch, "sender": sender,
			"round": repgen.followerState.r, "msgRound": msg.Round})
		return
	}
	if repgen.followerState.sentEcho != nil {
		repgen.logger.Debug("MessageFinal after already sent MessageFinalEcho", nil)
		return
	}
	if !repgen.verifyAttestedReport(msg.Report, sender) {
		return
	}
	repgen.followerState.sentEcho = &msg.Report
	repgen.netSender.Broadcast(MessageFinalEcho{MessageFinal: msg})
}

// messageFinalEcho is called when the local oracle process receives a
// "final-echo" message. If the report it contains is valid and the round is not
// yet complete, it keeps track of how many such echos have been received, and
// invokes the "transmit" event when enough echos have been seen to ensure that
// at least one (other?) honest node is broadcasting this report. This completes
// the round, from the local oracle's perspective.
func (repgen *reportGenerationState) messageFinalEcho(msg MessageFinalEcho,
	sender types.OracleID,
) {
	if msg.Epoch != repgen.e {
		repgen.logger.Debug("wrong epoch from MessageFinalEcho", types.LogFields{
			"round": repgen.followerState.r, "msgEpoch": msg.Epoch, "sender": sender})
		return
	}
	if msg.Round != repgen.followerState.r {
		repgen.logger.Debug("wrong round from MessageFinalEcho", types.LogFields{
			"round": repgen.followerState.r, "msgRound": msg.Round, "sender": sender})
		return
	}
	if repgen.followerState.receivedEcho[sender] {
		repgen.logger.Warn("extra MessageFinalEcho received", types.LogFields{
			"round": repgen.followerState.r, "sender": sender})
		return
	}
	if repgen.followerState.completedRound {
		repgen.logger.Debug("received final echo after round completion", nil)
		return
	}
	if !repgen.verifyAttestedReport(msg.Report, sender) { // if verify-attested-report(O) then
		// log messages are in verifyAttestedReport
		return
	}
	repgen.followerState.receivedEcho[sender] = true // receivedecho[j] ??? true

	if repgen.followerState.sentEcho == nil { // if sentecho = ??? then
		repgen.followerState.sentEcho = &msg.Report // sentecho ??? O
		repgen.netSender.Broadcast(msg)             // send [ FINALECHO , r, O] to all p_j ??? P
	}

	// upon {p j ??? P | receivedecho[j] = true} > f ??? ??completedround do
	{
		count := 0 // FUTUREWORK: Make this constant-time with a stateful counter
		for _, receivedEcho := range repgen.followerState.receivedEcho {
			if receivedEcho {
				count++
			}
		}
		if repgen.config.F < count {
			select {
			case repgen.chReportGenerationToTransmission <- EventTransmit{
				repgen.e,
				repgen.followerState.r,
				*repgen.followerState.sentEcho,
			}:
			case <-repgen.ctx.Done():
			}
			repgen.completeRound()
		}
	}

}

// observeValue is called when the oracle needs to gather a fresh observation to
// send back to the current leader.
func (repgen *reportGenerationState) observeValue() observation.Observation {
	var value observation.Observation
	var err error
	// We don't trust datasource.Observe(ctx) to actually exit after the context deadline.
	// We want to make sure we don't wait too long in order to not drop out of the
	// protocol. Even if an instance cannot make observations, it can still be useful,
	// e.g. by signing reports.
	ok := repgen.subprocesses.BlockForAtMost(
		repgen.ctx,
		repgen.localConfig.DataSourceTimeout,
		func(ctx context.Context) {
			var rawValue types.Observation
			rawValue, err = repgen.datasource.Observe(ctx)
			if err != nil {
				return
			}
			value, err = observation.MakeObservation((*big.Int)(rawValue))
		},
	)

	if !ok {
		repgen.logger.Error("DataSource timed out", types.LogFields{
			"round":   repgen.followerState.r,
			"timeout": repgen.localConfig.DataSourceTimeout,
		})
		return observation.Observation{}
	}

	if err != nil {
		repgen.logger.Error("DataSource errored", types.LogFields{
			"round": repgen.followerState.r,
			"error": err,
		})
		return observation.Observation{}
	}

	return value
}

func (repgen *reportGenerationState) shouldReport(observations []AttributedSignedObservation) bool {
	ctx, cancel := context.WithTimeout(repgen.ctx, repgen.localConfig.BlockchainTimeout)
	defer cancel()
	contractConfigDigest, contractEpoch, contractRound, rawAnswer, timestamp,
		err := repgen.contractTransmitter.LatestTransmissionDetails(ctx)
	if err != nil {
		repgen.logger.Error("shouldReport: Error during LatestTransmissionDetails", types.LogFields{
			"round": repgen.followerState.r,
			"error": err,
		})
		// Err on the side of creating too many reports. For instance, the Ethereum node
		// might be down, but that need not prevent us from still contributing to the
		// protocol.
		return true
	}

	answer, err := observation.MakeObservation(rawAnswer)
	if err != nil {
		repgen.logger.Error("shouldReport: Error during observation.NewObservation", types.LogFields{
			"round": repgen.followerState.r,
			"error": err,
		})
		return false
	}

	initialRound := contractConfigDigest == repgen.config.ConfigDigest && contractEpoch == 0 && contractRound == 0
	deviation := observations[len(observations)/2].SignedObservation.Observation.Deviates(answer, repgen.config.AlphaPPB)
	deltaCTimeout := timestamp.Add(repgen.config.DeltaC).Before(time.Now())
	result := initialRound || deviation || deltaCTimeout

	repgen.logger.Info("shouldReport: returning result", types.LogFields{
		"round":         repgen.followerState.r,
		"result":        result,
		"initialRound":  initialRound,
		"deviation":     deviation,
		"deltaCTimeout": deltaCTimeout,
	})

	return result
}

// completeRound is called by the local report-generation process when the
// current round has been completed by either concluding that the report sent by
// the current leader should not be transmitted to the on-chain smart contract,
// or by initiating the transmission protocol with this report.
func (repgen *reportGenerationState) completeRound() {
	repgen.logger.Debug("ReportGeneration: completed round", types.LogFields{
		"round": repgen.followerState.r,
	})
	repgen.followerState.completedRound = true

	select {
	case repgen.chReportGenerationToPacemaker <- EventProgress{}:
	case <-repgen.ctx.Done():
	}
}

// verifyReportReq errors unless the reports observations are sorted, its
// signatures are all correct given the current round/epoch/config, and from
// distinct oracles, and there are more than 2f observations.
func (repgen *reportGenerationState) verifyReportReq(msg MessageReportReq) error {
	// check sortedness
	if !sort.SliceIsSorted(msg.AttributedSignedObservations,
		func(i, j int) bool {
			return msg.AttributedSignedObservations[i].SignedObservation.Observation.Less(msg.AttributedSignedObservations[j].SignedObservation.Observation)
		}) {
		return errors.Errorf("messages not sorted by value")
	}

	// check signatures and signature distinctness
	{
		counted := map[types.OracleID]bool{}
		for _, obs := range msg.AttributedSignedObservations {
			// NOTE: OracleID is untrusted, therefore we _must_ bounds check it first
			numOracles := len(repgen.config.OracleIdentities)
			if int(obs.Observer) < 0 || numOracles <= int(obs.Observer) {
				return errors.Errorf("given oracle ID of %v is out of bounds (only "+
					"have %v public keys)", obs.Observer, numOracles)
			}
			if counted[obs.Observer] {
				return errors.Errorf("duplicate observation by oracle id %v", obs.Observer)
			} else {
				counted[obs.Observer] = true
			}
			observerOffchainPublicKey := repgen.config.OracleIdentities[obs.Observer].OffchainPublicKey
			if err := obs.SignedObservation.Verify(repgen.followerReportContext(), observerOffchainPublicKey); err != nil {
				return errors.Errorf("invalid signed observation: %s", err)
			}
		}
		bound := 2 * repgen.config.F
		if len(counted) <= bound {
			return errors.Errorf("not enough observations in report; got %d, "+
				"need more than %d", len(counted), bound)
		}
	}
	return nil
}

// verifyAttestedReport returns true iff the signatures on msg are valid
// signatures by oracle participants
func (repgen *reportGenerationState) verifyAttestedReport(
	report AttestedReportMany, sender types.OracleID,
) bool {
	if len(report.Signatures) <= repgen.config.F {
		repgen.logger.Warn("verifyAttestedReport: dropping final report because "+
			"it has too few signatures", types.LogFields{"sender": sender,
			"numSignatures": len(report.Signatures), "F": repgen.config.F})
		return false
	}

	keys := make(signature.EthAddresses)
	for oid, id := range repgen.config.OracleIdentities {
		keys[types.OnChainSigningAddress(id.OnChainSigningAddress)] =
			types.OracleID(oid)
	}

	err := report.VerifySignatures(repgen.followerReportContext(), keys)
	if err != nil {
		repgen.logger.Error("could not validate signatures on final report",
			types.LogFields{
				"round":  repgen.followerState.r,
				"error":  err,
				"report": report,
				"sender": sender,
			})
		return false
	}
	return true
}
