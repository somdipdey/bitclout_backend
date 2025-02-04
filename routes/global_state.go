package routes

import (
	"bytes"
	"encoding/json"
	"fmt"
	"github.com/bitclout/core/lib"
	"io"
	"net/http"
	"strings"

	"github.com/dgraph-io/badger/v3"
	"github.com/nyaruka/phonenumbers"
	"github.com/pkg/errors"
)

const (
	GlobalStateSharedSecretParam = "shared_secret"

	RoutePathGlobalStatePutRemote      = "/api/v1/global-state/put"
	RoutePathGlobalStateGetRemote      = "/api/v1/global-state/get"
	RoutePathGlobalStateBatchGetRemote = "/api/v1/global-state/batch-get"
	RoutePathGlobalStateDeleteRemote   = "/api/v1/global-state/delete"
	RoutePathGlobalStateSeekRemote     = "/api/v1/global-state/seek"
)

// GlobalStateRoutes returns the routes for managing global state.
// Note that these routes are generally protected by a shared_secret
func (fes *APIServer) GlobalStateRoutes() []Route {
	var GlobalStateRoutes = []Route{
		{
			"GlobalStatePutRemote",
			[]string{"POST", "OPTIONS"},
			RoutePathGlobalStatePutRemote,
			fes.GlobalStatePutRemote,
			true, // CheckSecret
		},
		{
			"GlobalStateGetRemote",
			[]string{"POST", "OPTIONS"},
			RoutePathGlobalStateGetRemote,
			fes.GlobalStateGetRemote,
			true, // CheckSecret
		},
		{
			"GlobalStateBatchGetRemote",
			[]string{"POST", "OPTIONS"},
			RoutePathGlobalStateBatchGetRemote,
			fes.GlobalStateBatchGetRemote,
			true, // CheckSecret
		},
		{
			"GlobalStateDeleteRemote",
			[]string{"POST", "OPTIONS"},
			RoutePathGlobalStateDeleteRemote,
			fes.GlobalStateDeleteRemote,
			true, // CheckSecret
		},
		{
			"GlobalStateSeekRemote",
			[]string{"POST", "OPTIONS"},
			RoutePathGlobalStateSeekRemote,
			fes.GlobalStateSeekRemote,
			true, // CheckSecret
		},
	}

	return GlobalStateRoutes
}

var (
	// The key prefixes for the  global state key-value database.

	// The prefix for accessing a user's metadata (e.g. email, blacklist status, etc.):
	// <prefix,  ProfilePubKey [33]byte> -> <UserMetadata>
	_GlobalStatePrefixPublicKeyToUserMetadata = []byte{0}

	// The prefix for accessing whitelisted posts for the global feed:
	// Unlike in db_utils here, we use a single byte as a value placeholder.  This is a
	// result of the way global state handles when a key is not present.
	// <prefix, tstampNanos uint64, PostHash> -> <[]byte{1}>
	_GlobalStatePrefixTstampNanosPostHash = []byte{1}

	// The prefix for accessing a phone number's metadata
	// <prefix,  PhoneNumber [variableLength]byte> -> <PhoneNumberMetadata>
	_GlobalStatePrefixPhoneNumberToPhoneNumberMetadata = []byte{2}

	// The prefix for accessing the verified users map.
	// The resulting map takes a username and returns a PKID.
	// <prefix> -> <map[string]*PKID>
	_GlobalStatePrefixForVerifiedMap = []byte{3}

	// The prefix for accessing the pinned posts on the global feed:
	// <prefix, tstampNanos uint64, PostHash> -> <[]byte{4}>
	_GlobalStatePrefixTstampNanosPinnedPostHash = []byte{4}

	// The prefix for accessing the audit log of verification badges
	// <prefix, username string> -> <VerificationAuditLog>
	_GlobalStatePrefixUsernameVerificationAuditLog = []byte{5}


	// The prefix for accessing the graylisted users.
	// <prefix, public key> -> <IsGraylisted>
	_GlobalStatePrefixPublicKeyToGraylistState = []byte{6}

	// The prefix for accesing the blacklisted users.
	// <prefix, public key> -> <IsBlacklisted>
	_GlobalStatePrefixPublicKeyToBlacklistState = []byte{7}

	// The prefix for checking the most recent read time stamp for a user reading
	// a contact's private message.
	// <prefix, user public key, contact's public key> -> <tStampNanos>
	_GlobalStatePrefixUserPublicKeyContactPublicKeyToMostRecentReadTstampNanos = []byte{8}

	// TODO: This process is a bit error-prone. We should come up with a test or
	// something to at least catch cases where people have two prefixes with the
	// same ID.
	//
	// NEXT_TAG: 3
)

// This struct contains all the metadata associated with a user's public key.
type UserMetadata struct {
	// The PublicKey of the user this metadata is associated with.
	PublicKey []byte

	// True if this user should be hidden from all data returned to the app.
	RemoveEverywhere bool

	// True if this user should be hidden from the creator leaderboard.
	RemoveFromLeaderboard bool

	// Email address for a user to receive email notifications at.
	Email string

	// E.164 format phone number for a user to receive text notifications at.
	PhoneNumber string

	// Country code associated with the user's phone number. This is a string like "US"
	PhoneNumberCountryCode string

	// This map stores the number of messages that a user has read from a specific contact.
	// The map is indexed with the contact's PublicKeyBase58Check and maps to an integer
	// number of messages that the user has read.
	MessageReadStateByContact map[string]int

	// Store the index of the last notification that the user saw
	NotificationLastSeenIndex int64

	// Amount of Bitcoin that users have burned so far via the Buy BitClout UI
	//
	// We track this so that, if the user does multiple burns,
	// we can set HasBurnedEnoughSatoshisToCreateProfile based on the total
	//
	// This tracks the "total input satoshis" (i.e. it includes fees the user spends).
	// Including fees makes it less expensive for a user to make a profile. We're cutting
	// users a break, but we could change this later.
	SatoshisBurnedSoFar uint64

	// True if the user has burned enough satoshis to create a profile. This can be
	// set to true from the BurnBitcoinStateless endpoint or canUserCreateProfile.
	//
	// We store this (instead of computing it when the user loads the page) to avoid issues
	// where the user burns the required amount, and then we reboot the node and change the min
	// satoshis required, and then the user hasn't burned enough. Once a user has burned enough,
	// we want him to be allowed to create a profile forever.
	HasBurnedEnoughSatoshisToCreateProfile bool

	// Map of public keys of profiles this user has blocked.  The map here functions as a hashset to make look ups more
	// efficient.  Values are empty structs to keep memory usage down.
	BlockedPublicKeys map[string]struct{}

	// If true, this user's posts will automatically be added to the global whitelist (max 5 per day).
	WhitelistPosts bool
}

// This struct contains all the metadata associated with a user's phone number.
type PhoneNumberMetadata struct {
	// The PublicKey of the user that this phone number belongs to.
	PublicKey []byte

	// E.164 format phone number for a user to receive text notifications at.
	PhoneNumber string

	// Country code associated with the user's phone number.
	PhoneNumberCountryCode string

	// if true, when the public key associated with this metadata tries to create a profile, we will comp their fee.
	ShouldCompProfileCreation bool
}

// countryCode is a string like 'US' (Note: the phonenumbers lib calls this a "region code")
func GlobalStateKeyForPhoneNumberStringToPhoneNumberMetadata(phoneNumber string) (_key []byte, _err error) {
	parsedNumber, err := phonenumbers.Parse(phoneNumber, "")
	if err != nil {
		return nil, errors.Wrap(fmt.Errorf(
			"GlobalStateKeyForPhoneNumberStringToPhoneNumberMetadata: Problem with phonenumbers.Parse: %v", err), "")
	}
	formattedNumber := phonenumbers.Format(parsedNumber, phonenumbers.E164)

	// Get the key for the formatted number
	return globalStateKeyForPhoneNumberBytesToPhoneNumberMetadata([]byte(formattedNumber)), nil
}

// Key for accessing a user's global metadata.
// External callers should use GlobalStateKeyForPhoneNumberStringToPhoneNumberMetadata, not this function,
// to ensure that the phone number key is formatted in a standard way
func globalStateKeyForPhoneNumberBytesToPhoneNumberMetadata(phoneNumberBytes []byte) []byte {
	prefixCopy := append([]byte{}, _GlobalStatePrefixPhoneNumberToPhoneNumberMetadata...)
	key := append(prefixCopy, phoneNumberBytes[:]...)
	return key
}

// Key for accessing a user's global metadata.
func GlobalStateKeyForPublicKeyToUserMetadata(profilePubKey []byte) []byte {
	prefixCopy := append([]byte{}, _GlobalStatePrefixPublicKeyToUserMetadata...)
	key := append(prefixCopy, profilePubKey[:]...)
	return key
}

// Key for accessing a whitelised post in the global feed index.
func GlobalStateKeyForTstampPostHash(tstampNanos uint64, postHash *lib.BlockHash) []byte {
	// Make a copy to avoid multiple calls to this function re-using the same slice.
	key := append([]byte{}, _GlobalStatePrefixTstampNanosPostHash...)
	key = append(key, lib.EncodeUint64(tstampNanos)...)
	key = append(key, postHash[:]...)
	return key
}

// Key for accessing a pinned post.
func GlobalStateKeyForTstampPinnedPostHash(tstampNanos uint64, postHash *lib.BlockHash) []byte {
	// Make a copy to avoid multiple calls to this function re-using the same slice.
	key := append([]byte{}, _GlobalStatePrefixTstampNanosPinnedPostHash...)
	key = append(key, lib.EncodeUint64(tstampNanos)...)
	key = append(key, postHash[:]...)
	return key
}

// Key for accessing verification audit logs for a given username
func GlobalStateKeyForUsernameVerificationAuditLogs(username string) []byte {
	key := append([]byte{}, _GlobalStatePrefixUsernameVerificationAuditLog...)
	key = append(key, []byte(strings.ToLower(username))...)
	return key
}

// Key for accessing a graylisted user.
func GlobalStateKeyForGraylistedProfile(profilePubKey []byte) []byte {
	key := append([]byte{}, _GlobalStatePrefixPublicKeyToGraylistState...)
	key = append(key, profilePubKey...)
	return key
}

// Key for accessing a blacklisted user.
func GlobalStateKeyForBlacklistedProfile(profilePubKey []byte) []byte {
	key := append([]byte{}, _GlobalStatePrefixPublicKeyToBlacklistState...)
	key = append(key, profilePubKey...)
	return key
}

// Key for accessing a user's global metadata.
func GlobalStateKeyForUserPkContactPkToMostRecentReadTstampNanos(userPubKey []byte, contactPubKey []byte) []byte {
	prefixCopy := append([]byte{}, _GlobalStatePrefixUserPublicKeyContactPublicKeyToMostRecentReadTstampNanos...)
	key := append(prefixCopy, userPubKey[:]...)
	key = append(key, contactPubKey[:]...)
	return key
}


type GlobalStatePutRemoteRequest struct {
	Key   []byte
	Value []byte
}

type GlobalStatePutRemoteResponse struct {
}

func (fes *APIServer) GlobalStatePutRemote(ww http.ResponseWriter, rr *http.Request) {
	// Parse the request.
	decoder := json.NewDecoder(io.LimitReader(rr.Body, MaxRequestBodySizeBytes))
	requestData := GlobalStatePutRemoteRequest{}
	if err := decoder.Decode(&requestData); err != nil {
		_AddBadRequestError(ww, fmt.Sprintf("GlobalStatePutRemote: Problem parsing request body: %v", err))
		return
	}

	// Call the put function. Note that this may also proxy to another node.
	if err := fes.GlobalStatePut(requestData.Key, requestData.Value); err != nil {
		_AddBadRequestError(ww, fmt.Sprintf(
			"GlobalStatePutRemote: Error processing GlobalStatePut: %v", err))
		return
	}

	// Return
	res := GlobalStatePutRemoteResponse{}
	if err := json.NewEncoder(ww).Encode(res); err != nil {
		_AddBadRequestError(ww, fmt.Sprintf("GlobalStatePutRemote: Problem encoding response as JSON: %v", err))
		return
	}
}

func (fes *APIServer) CreateGlobalStatePutRequest(key []byte, value []byte) (
	_url string, _json_data []byte, _err error) {

	req := GlobalStatePutRemoteRequest{
		Key:   key,
		Value: value,
	}
	json_data, err := json.Marshal(req)
	if err != nil {
		return "", nil, fmt.Errorf("GlobalStatePut: Could not marshal JSON: %v", err)
	}

	url := fmt.Sprintf("%s%s?%s=%s",
		fes.GlobalStateRemoteNode, RoutePathGlobalStatePutRemote,
		GlobalStateSharedSecretParam, fes.GlobalStateRemoteNodeSharedSecret)

	return url, json_data, nil
}

func (fes *APIServer) GlobalStatePut(key []byte, value []byte) error {
	// If we have a remote node then use that node to fulfill this request.
	if fes.GlobalStateRemoteNode != "" {
		// TODO: This codepath is hard to exercise in a test.

		url, json_data, err := fes.CreateGlobalStatePutRequest(key, value)
		if err != nil {
			return fmt.Errorf("GlobalStatePut: Error constructing request: %v", err)
		}
		res, err := http.Post(
			url,
			"application/json", /*contentType*/
			bytes.NewBuffer(json_data))
		if err != nil {
			return fmt.Errorf("GlobalStatePut: Error processing remote request")
		}
		res.Body.Close()

		//res := GlobalStatePutRemoteResponse{}
		//json.NewDecoder(resReturned.Body).Decode(&res)

		// No error means nothing to return.
		return nil
	}

	// If we get here, it means we don't have a remote node so store the
	// data in our local db.
	return fes.GlobalStateDB.Update(func(txn *badger.Txn) error {
		return txn.Set(key, value)
	})
}

type GlobalStateGetRemoteRequest struct {
	Key []byte
}

type GlobalStateGetRemoteResponse struct {
	Value []byte
}

func (fes *APIServer) GlobalStateGetRemote(ww http.ResponseWriter, rr *http.Request) {
	// Parse the request.
	decoder := json.NewDecoder(io.LimitReader(rr.Body, MaxRequestBodySizeBytes))
	requestData := GlobalStateGetRemoteRequest{}
	if err := decoder.Decode(&requestData); err != nil {
		_AddBadRequestError(ww, fmt.Sprintf("GlobalStateGetRemote: Problem parsing request body: %v", err))
		return
	}

	// Call the get function. Note that this may also proxy to another node.
	val, err := fes.GlobalStateGet(requestData.Key)
	if err != nil {
		_AddBadRequestError(ww, fmt.Sprintf(
			"GlobalStateGetRemote: Error processing GlobalStateGet: %v", err))
		return
	}

	// Return
	res := GlobalStateGetRemoteResponse{
		Value: val,
	}
	if err := json.NewEncoder(ww).Encode(res); err != nil {
		_AddBadRequestError(ww, fmt.Sprintf("GlobalStateGetRemote: Problem encoding response as JSON: %v", err))
		return
	}
}

func (fes *APIServer) CreateGlobalStateGetRequest(key []byte) (
	_url string, _json_data []byte, _err error) {

	req := GlobalStateGetRemoteRequest{
		Key: key,
	}
	json_data, err := json.Marshal(req)
	if err != nil {
		return "", nil, fmt.Errorf("GlobalStateGet: Could not marshal JSON: %v", err)
	}

	url := fmt.Sprintf("%s%s?%s=%s",
		fes.GlobalStateRemoteNode, RoutePathGlobalStateGetRemote,
		GlobalStateSharedSecretParam, fes.GlobalStateRemoteNodeSharedSecret)

	return url, json_data, nil
}

func (fes *APIServer) GlobalStateGet(key []byte) (value []byte, _err error) {
	// If we have a remote node then use that node to fulfill this request.
	if fes.GlobalStateRemoteNode != "" {
		// TODO: This codepath is currently annoying to test.

		url, json_data, err := fes.CreateGlobalStateGetRequest(key)
		if err != nil {
			return nil, fmt.Errorf(
				"GlobalStateGet: Error constructing request: %v", err)
		}

		resReturned, err := http.Post(
			url,
			"application/json", /*contentType*/
			bytes.NewBuffer(json_data))
		if err != nil {
			return nil, fmt.Errorf("GlobalStateGet: Error processing remote request")
		}

		res := GlobalStateGetRemoteResponse{}
		json.NewDecoder(resReturned.Body).Decode(&res)
		resReturned.Body.Close()

		return res.Value, nil
	}

	// If we get here, it means we don't have a remote node so get the
	// data from our local db.
	var retValue []byte
	err := fes.GlobalStateDB.View(func(txn *badger.Txn) error {
		item, err := txn.Get(key)
		if err != nil {
			return nil
		}
		retValue, err = item.ValueCopy(nil)
		if err != nil {
			return err
		}

		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("GlobalStateGet: Error copying value into new slice: %v", err)
	}

	return retValue, nil
}

type GlobalStateBatchGetRemoteRequest struct {
	KeyList [][]byte
}

type GlobalStateBatchGetRemoteResponse struct {
	ValueList [][]byte
}

func (fes *APIServer) GlobalStateBatchGetRemote(ww http.ResponseWriter, rr *http.Request) {
	// Parse the request.
	decoder := json.NewDecoder(io.LimitReader(rr.Body, MaxRequestBodySizeBytes))
	requestData := GlobalStateBatchGetRemoteRequest{}
	if err := decoder.Decode(&requestData); err != nil {
		_AddBadRequestError(ww, fmt.Sprintf("GlobalStateBatchGetRemote: Problem parsing request body: %v", err))
		return
	}

	// Call the get function. Note that this may also proxy to another node.
	values, err := fes.GlobalStateBatchGet(requestData.KeyList)
	if err != nil {
		_AddBadRequestError(ww, fmt.Sprintf(
			"GlobalStateBatchGetRemote: Error processing GlobalStateBatchGet: %v", err))
		return
	}

	// Return
	res := GlobalStateBatchGetRemoteResponse{
		ValueList: values,
	}
	if err := json.NewEncoder(ww).Encode(res); err != nil {
		_AddBadRequestError(ww, fmt.Sprintf("GlobalStateBatchGetRemote: Problem encoding response as JSON: %v", err))
		return
	}
}

func (fes *APIServer) CreateGlobalStateBatchGetRequest(keyList [][]byte) (
	_url string, _json_data []byte, _err error) {

	req := GlobalStateBatchGetRemoteRequest{
		KeyList: keyList,
	}
	json_data, err := json.Marshal(req)
	if err != nil {
		return "", nil, fmt.Errorf("GlobalStateBatchGet: Could not marshal JSON: %v", err)
	}

	url := fmt.Sprintf("%s%s?%s=%s",
		fes.GlobalStateRemoteNode, RoutePathGlobalStateBatchGetRemote,
		GlobalStateSharedSecretParam, fes.GlobalStateRemoteNodeSharedSecret)

	return url, json_data, nil
}

func (fes *APIServer) GlobalStateBatchGet(keyList [][]byte) (value [][]byte, _err error) {
	// If we have a remote node then use that node to fulfill this request.
	if fes.GlobalStateRemoteNode != "" {
		// TODO: This codepath is currently annoying to test.

		url, json_data, err := fes.CreateGlobalStateBatchGetRequest(keyList)
		if err != nil {
			return nil, fmt.Errorf(
				"GlobalStateBatchGet: Error constructing request: %v", err)
		}

		resReturned, err := http.Post(
			url,
			"application/json", /*contentType*/
			bytes.NewBuffer(json_data))
		if err != nil {
			return nil, fmt.Errorf("GlobalStateBatchGet: Error processing remote request")
		}

		res := GlobalStateBatchGetRemoteResponse{}
		json.NewDecoder(resReturned.Body).Decode(&res)
		resReturned.Body.Close()

		return res.ValueList, nil
	}

	// If we get here, it means we don't have a remote node so get the
	// data from our local db.
	var retValueList [][]byte
	err := fes.GlobalStateDB.View(func(txn *badger.Txn) error {
		for _, key := range keyList {
			item, err := txn.Get(key)
			if err != nil {
				retValueList = append(retValueList, []byte{})
				continue
			}
			value, err := item.ValueCopy(nil)
			if err != nil {
				return err
			} else {
				retValueList = append(retValueList, value)
			}
		}

		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("GlobalStateBatchGet: Error copying value into new slice: %v", err)
	}

	return retValueList, nil
}

type GlobalStateDeleteRemoteRequest struct {
	Key []byte
}

type GlobalStateDeleteRemoteResponse struct {
}

func (fes *APIServer) CreateGlobalStateDeleteRequest(key []byte) (
	_url string, _json_data []byte, _err error) {

	req := GlobalStateDeleteRemoteRequest{
		Key: key,
	}
	json_data, err := json.Marshal(req)
	if err != nil {
		return "", nil, fmt.Errorf("GlobalStateDelete: Could not marshal JSON: %v", err)
	}

	url := fmt.Sprintf("%s%s?%s=%s",
		fes.GlobalStateRemoteNode, RoutePathGlobalStateDeleteRemote,
		GlobalStateSharedSecretParam, fes.GlobalStateRemoteNodeSharedSecret)

	return url, json_data, nil
}

func (fes *APIServer) GlobalStateDeleteRemote(ww http.ResponseWriter, rr *http.Request) {
	// Parse the request.
	decoder := json.NewDecoder(io.LimitReader(rr.Body, MaxRequestBodySizeBytes))
	requestData := GlobalStateDeleteRemoteRequest{}
	if err := decoder.Decode(&requestData); err != nil {
		_AddBadRequestError(ww, fmt.Sprintf("GlobalStateDeleteRemote: Problem parsing request body: %v", err))
		return
	}

	// Call the Delete function. Note that this may also proxy to another node.
	if err := fes.GlobalStateDelete(requestData.Key); err != nil {
		_AddBadRequestError(ww, fmt.Sprintf(
			"GlobalStateDeleteRemote: Error processing GlobalStateDelete: %v", err))
		return
	}

	// Return
	res := GlobalStateDeleteRemoteResponse{}
	if err := json.NewEncoder(ww).Encode(res); err != nil {
		_AddBadRequestError(ww, fmt.Sprintf("GlobalStateDeleteRemote: Problem encoding response as JSON: %v", err))
		return
	}
}

func (fes *APIServer) GlobalStateDelete(key []byte) error {
	// If we have a remote node then use that node to fulfill this request.
	if fes.GlobalStateRemoteNode != "" {
		// TODO: This codepath is currently annoying to test.

		url, json_data, err := fes.CreateGlobalStateDeleteRequest(key)
		if err != nil {
			return fmt.Errorf("GlobalStateDelete: Could not construct request: %v", err)
		}

		res, err := http.Post(
			url,
			"application/json", /*contentType*/
			bytes.NewBuffer(json_data))
		if err != nil {
			return fmt.Errorf("GlobalStateDelete: Error processing remote request")
		}

		res.Body.Close()
		//res := GlobalStateDeleteRemoteResponse{}
		//json.NewDecoder(resReturned.Body).Decode(&res)

		// No error means nothing to return.
		return nil
	}

	// If we get here, it means we don't have a remote node so store the
	// data in our local db.
	return fes.GlobalStateDB.Update(func(txn *badger.Txn) error {
		return txn.Delete(key)
	})
}

type GlobalStateSeekRemoteRequest struct {
	StartPrefix    []byte
	ValidForPrefix []byte
	MaxKeyLen      int
	NumToFetch     int
	Reverse        bool
	FetchValues    bool
}
type GlobalStateSeekRemoteResponse struct {
	KeysFound [][]byte
	ValsFound [][]byte
}

func (fes *APIServer) CreateGlobalStateSeekRequest(startPrefix []byte, validForPrefix []byte,
	maxKeyLen int, numToFetch int, reverse bool, fetchValues bool) (
	_url string, _json_data []byte, _err error) {

	req := GlobalStateSeekRemoteRequest{
		StartPrefix:    startPrefix,
		ValidForPrefix: validForPrefix,
		MaxKeyLen:      maxKeyLen,
		NumToFetch:     numToFetch,
		Reverse:        reverse,
		FetchValues:    fetchValues,
	}
	json_data, err := json.Marshal(req)
	if err != nil {
		return "", nil, fmt.Errorf("GlobalStateSeek: Could not marshal JSON: %v", err)
	}

	url := fmt.Sprintf("%s%s?%s=%s",
		fes.GlobalStateRemoteNode, RoutePathGlobalStateSeekRemote,
		GlobalStateSharedSecretParam, fes.GlobalStateRemoteNodeSharedSecret)

	return url, json_data, nil
}
func (fes *APIServer) GlobalStateSeekRemote(ww http.ResponseWriter, rr *http.Request) {
	// Parse the request.
	decoder := json.NewDecoder(io.LimitReader(rr.Body, MaxRequestBodySizeBytes))
	requestData := GlobalStateSeekRemoteRequest{}
	if err := decoder.Decode(&requestData); err != nil {
		_AddBadRequestError(ww, fmt.Sprintf("GlobalStateSeekRemote: Problem parsing request body: %v", err))
		return
	}

	// Call the get function. Note that this may also proxy to another node.
	keys, values, err := fes.GlobalStateSeek(
		requestData.StartPrefix,
		requestData.ValidForPrefix,
		requestData.MaxKeyLen,
		requestData.NumToFetch,
		requestData.Reverse,
		requestData.FetchValues,
	)
	if err != nil {
		_AddBadRequestError(ww, fmt.Sprintf(
			"GlobalStateSeekRemote: Error processing GlobalStateSeek: %v", err))
		return
	}

	// Return
	res := GlobalStateSeekRemoteResponse{
		KeysFound: keys,
		ValsFound: values,
	}
	if err := json.NewEncoder(ww).Encode(res); err != nil {
		_AddBadRequestError(ww, fmt.Sprintf("GlobalStateSeekRemote: Problem encoding response as JSON: %v", err))
		return
	}
}

func (fes *APIServer) GlobalStateSeek(startPrefix []byte, validForPrefix []byte,
	maxKeyLen int, numToFetch int, reverse bool, fetchValues bool) (
	_keysFound [][]byte, _valsFound [][]byte, _err error) {

	// If we have a remote node then use that node to fulfill this request.
	if fes.GlobalStateRemoteNode != "" {
		// TODO: This codepath is currently annoying to test.

		url, json_data, err := fes.CreateGlobalStateSeekRequest(
			startPrefix,
			validForPrefix,
			maxKeyLen,
			numToFetch,
			reverse,
			fetchValues)
		if err != nil {
			return nil, nil, fmt.Errorf(
				"GlobalStateSeek: Error constructing request: %v", err)
		}

		resReturned, err := http.Post(
			url,
			"application/json", /*contentType*/
			bytes.NewBuffer(json_data))
		if err != nil {
			return nil, nil, fmt.Errorf("GlobalStateSeek: Error processing remote request")
		}

		res := GlobalStateSeekRemoteResponse{}
		json.NewDecoder(resReturned.Body).Decode(&res)
		resReturned.Body.Close()

		return res.KeysFound, res.ValsFound, nil
	}

	// If we get here, it means we don't have a remote node so get the
	// data from our local db.
	retKeys, retVals, err := lib.DBGetPaginatedKeysAndValuesForPrefix(fes.GlobalStateDB, startPrefix,
		validForPrefix, maxKeyLen, numToFetch, reverse, fetchValues)
	if err != nil {
		return nil, nil, fmt.Errorf("GlobalStateSeek: Error getting paginated keys and values: %v", err)
	}

	return retKeys, retVals, nil
}
