package protocol

import "github.com/SeerLink/libocr/offchainreporting/types"

// Used only for testing
type XXXUnknownMessageType struct{}

// Conform to protocol.Message interface
func (XXXUnknownMessageType) process(*oracleState, types.OracleID) {}
