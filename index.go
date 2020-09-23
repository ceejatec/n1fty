//  Copyright (c) 2019 Couchbase, Inc.
//  Licensed under the Apache License, Version 2.0 (the "License");
//  you may not use this file except in compliance with the
//  License. You may obtain a copy of the License at
//    http://www.apache.org/licenses/LICENSE-2.0
//  Unless required by applicable law or agreed to in writing,
//  software distributed under the License is distributed on an "AS
//  IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either
//  express or implied. See the License for the specific language
//  governing permissions and limitations under the License.

package n1fty

import (
	"context"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/blevesearch/bleve"
	"github.com/blevesearch/bleve/mapping"
	"github.com/couchbase/cbgt"
	"github.com/couchbase/n1fty/util"
	"github.com/couchbase/query/datastore"
	"github.com/couchbase/query/errors"
	"github.com/couchbase/query/expression"
	"github.com/couchbase/query/expression/parser"
	"github.com/couchbase/query/logging"
	"github.com/couchbase/query/timestamp"
	"github.com/couchbase/query/value"
)

const doneRequest = int64(1)

// FTSIndex implements datastore.FTSIndex interface
type FTSIndex struct {
	indexer  *FTSIndexer
	indexDef *cbgt.IndexDef

	// map of SearchFields to dynamic-ness
	searchableFields map[util.SearchField]bool
	// number of indexed fields
	indexedCount int64

	condExpr expression.Expression

	dynamic            bool // true if a top-level dynamic mapping is enabled
	allFieldSearchable bool // true if _all field contains some content

	defaultAnalyzer       string
	defaultDateTimeParser string
}

// -----------------------------------------------------------------------------

func newFTSIndex(indexer *FTSIndexer,
	indexDef *cbgt.IndexDef,
	searchableFields map[util.SearchField]bool,
	indexedCount int64,
	condExprStr string,
	dynamic bool,
	allFieldSearchable bool,
	defaultAnalyzer string,
	defaultDateTimeParser string) (rv *FTSIndex, err error) {
	var condExpr expression.Expression
	if len(condExprStr) > 0 {
		condExpr, err = parser.Parse(condExprStr)
		if err != nil {
			return nil, err
		}
	}

	index := &FTSIndex{
		indexer:               indexer,
		indexDef:              indexDef,
		searchableFields:      searchableFields,
		indexedCount:          indexedCount,
		condExpr:              condExpr,
		dynamic:               dynamic,
		allFieldSearchable:    allFieldSearchable,
		defaultAnalyzer:       defaultAnalyzer,
		defaultDateTimeParser: defaultDateTimeParser,
	}

	return index, nil
}

// -----------------------------------------------------------------------------

func (i *FTSIndex) KeyspaceId() string {
	return i.indexer.KeyspaceId()
}

func (i *FTSIndex) Id() string {
	return i.indexDef.UUID
}

func (i *FTSIndex) Name() string {
	return i.indexDef.Name
}

func (i *FTSIndex) Type() datastore.IndexType {
	return datastore.FTS
}

func (i *FTSIndex) Indexer() datastore.Indexer {
	return i.indexer
}

func (i *FTSIndex) SeekKey() expression.Expressions {
	// not supported
	return nil
}

func (i *FTSIndex) RangeKey() expression.Expressions {
	// not supported
	return nil
}

func (i *FTSIndex) Condition() expression.Expression {
	return i.condExpr // Non-nil, for example, when 'type="beer"'.
}

func (i *FTSIndex) IsPrimary() bool {
	return false
}

func (i *FTSIndex) State() (datastore.IndexState, string, errors.Error) {
	return datastore.ONLINE, "", nil
}

func (i *FTSIndex) Statistics(requestID string, span *datastore.Span) (
	datastore.Statistics, errors.Error) {
	return nil, util.N1QLError(nil, "Statistics not supported yet")
}

func (i *FTSIndex) Drop(requestID string) errors.Error {
	return util.N1QLError(nil, "Drop not supported")
}

func (i *FTSIndex) Scan(requestID string, span *datastore.Span, distinct bool,
	limit int64, cons datastore.ScanConsistency,
	vector timestamp.Vector, conn *datastore.IndexConnection) {
	conn.Error(util.N1QLError(nil, "Scan not supported"))
	return
}

// Search performs a search/scan over this index, with provided SearchInfo settings
func (i *FTSIndex) Search(requestID string, searchInfo *datastore.FTSSearchInfo,
	cons datastore.ScanConsistency, vector timestamp.Vector,
	conn *datastore.IndexConnection) {
	if util.Debug > 0 {
		logging.Infof("n1fty: Search, index: %s, requestID: %s, searchInfo: %+v,"+
			" cons: %v, vector: %v\n",
			i.indexDef.Name, requestID, searchInfo, cons, vector)
	}

	if conn == nil {
		return
	}

	sender := conn.Sender()

	if sender == nil {
		conn.Error(util.N1QLError(nil, "conn's Sender not defined"))
		return
	}

	if searchInfo == nil || searchInfo.Query == nil {
		conn.Error(util.N1QLError(nil, "no search parameters provided"))
		sender.Close()
		return
	}

	if cons == datastore.SCAN_PLUS {
		conn.Error(util.N1QLError(nil, "scan_plus consistency not supported"))
		sender.Close()
		return
	}

	field := ""
	if searchInfo.Field != nil {
		if fieldStr, ok := searchInfo.Field.Actual().(string); ok {
			field = fieldStr
		} else {
			conn.Error(util.N1QLError(nil, "field provided must be of type:string"))
			sender.Close()
			return
		}
	}

	// this sargable(...) check is to ensure that the query is indeed "sargable"
	// at search time, as when the Sargable(..) API is invoked during the
	// prepare time, the query/options may not have been available.
	sargRV := i.buildQueryAndCheckIfSargable(
		field, searchInfo.Query, searchInfo.Options, nil)
	if sargRV.err != nil || sargRV.count == 0 {
		conn.Error(util.N1QLError(nil, "not sargable"))
		sender.Close()
		return
	}

	starttm := time.Now()

	var waitGroup sync.WaitGroup
	var backfillSync int64
	var rh *responseHandler
	var err error
	ctx, cancel := context.WithCancel(context.Background())

	defer func() {
		atomic.StoreInt64(&backfillSync, doneRequest)
		waitGroup.Wait()
		sender.Close()
		cancel()
		// cleanup the backfill file
		if rh != nil {
			rh.cleanupBackfill()
		}
	}()

	searchReq, err := util.BuildProtoSearchRequest(sargRV.searchRequest,
		searchInfo,
		vector, cons, i.indexDef.Name)
	if err != nil {
		conn.Error(util.N1QLError(err, "search request parse err"))
		return
	}

	ftsClient := i.indexer.getClient()
	if ftsClient == nil {
		conn.Error(util.N1QLError(nil, "client unavailable, try refreshing"))
		return
	}

	client := ftsClient.getGrpcClient()
	if client == nil {
		conn.Error(util.N1QLError(nil, "gRPC client unavailable, try refreshing"))
		return
	}

	stream, err := client.Search(ctx, searchReq)
	if err != nil || stream == nil {
		conn.Error(util.N1QLError(err, "search failed"))
		return
	}

	rh = newResponseHandler(i, requestID, sargRV.searchRequest)

	rh.handleResponse(conn, &waitGroup, &backfillSync, stream)

	atomic.AddInt64(&i.indexer.stats.TotalSearch, 1)
	atomic.AddInt64(&i.indexer.stats.TotalSearchDuration, int64(time.Since(starttm)))
}

// -----------------------------------------------------------------------------

type sargableRV struct {
	count         int
	indexedCount  int64
	opaque        map[string]interface{}
	searchRequest *bleve.SearchRequest
	err           errors.Error
}

// Sargable checks if the provided request is applicable for the index.
//
// Return parameters:
// - sargable_count: This is the number of fields whose names along with
//                   analyzers from the built query matched with that of
//                   the index definition, for now all of query fields or 0.
// - indexed_count:  This is the total number of indexed fields within the
//                   the FTS index.
// - exact:          True if the query would produce no false positives
//                   using this FTS index.
//                   (place holder for when partial sargability is supported)
// - opaque:         The map of certain contextual data that can be re-used
//                   as query iterates through several FTSIndexes.
//                   (in-out parameter)
//
// Contents of opaque:
//     - an entry for query field-type-analyzers
//     - an entry for searchable fields obtained from index option
//     - an entry for the search request generated from the query & field.
//
// The caller will have to make the decision on which index to choose based
// on the sargable_count (higher the better), indexed_count (lower the better),
// and exact (if true) returned.
func (i *FTSIndex) Sargable(field string, query,
	options expression.Expression, opaque interface{}) (
	int, int64, bool, interface{}, errors.Error) {
	var queryVal, optionsVal value.Value
	if query != nil {
		queryVal = query.Value()
	}
	if options != nil {
		optionsVal = options.Value()
	}

	// for now, exact will always be true (to prevent n1ql from doing
	// unnecessary KV fetches);
	// this is more of a place holder for until partial sargability is
	// supported where n1fty can determine whether a particular index
	// would generate false positives or not for a given query.
	exact := true

	var queryFields map[util.SearchField]struct{}
	if opq, ok := opaque.(map[string]interface{}); ok {
		if _, exists := opq["query_fields"]; exists {
			queryFields, _ = opq["query_fields"].(map[util.SearchField]struct{})
		}
	}

	if queryVal == nil && len(queryFields) == 0 {
		// this index will be sargable for the unavailable query if
		// it has a default dynamic mapping with the _all field searchable.
		if i.dynamic && i.allFieldSearchable {
			return int(math.MaxInt64), math.MaxInt64, exact, opaque, nil
		}

		// if the index isn't default dynamic, check if the query expression
		// provided is an object construct, so as to retrieve any available
		// "field" information;
		// use available field information to determine the sargability of
		// this index for the field(s), ignoring the analyzer, type etc. for
		// now; sargability is tested for again during search time when the
		// query becomes available.
		queryFields = map[util.SearchField]struct{}{}

		var fetchFields func(expression.Expression)
		fetchFields = func(arg expression.Expression) {
			if oc, ok := arg.(*expression.ObjectConstruct); ok {
				for name, val := range oc.Mapping() {
					n := name.Value()
					if n != nil &&
						n.Type() == value.STRING && n.Actual().(string) == "field" {
						if val.Value() != nil && val.Value().Type() == value.STRING {
							queryFields[util.SearchField{
								Name: val.Value().Actual().(string),
							}] = struct{}{}
						}
					} else {
						fetchFields(val)
					}
				}
			} else if ac, ok := arg.(*expression.ArrayConstruct); ok {
				for _, entry := range ac.Operands() {
					fetchFields(entry)
				}
			}
		}

		fetchFields(query)

		if len(queryFields) > 0 {
			opq, ok := opaque.(map[string]interface{})
			if !ok {
				opq = make(map[string]interface{})
			}
			opq["query_fields"] = queryFields
			opaque = opq
		}
	}

	rv := i.buildQueryAndCheckIfSargable(field, queryVal, optionsVal, opaque)

	if util.Debug > 0 {
		logging.Infof("n1fty: Sargable, index: %s, field: %s, query: %v,"+
			" options: %v, rv: %+v, exact: %t",
			i.indexDef.Name, field, query, options, rv, exact)
	}

	return rv.count, rv.indexedCount, exact, rv.opaque, rv.err
}

func (i *FTSIndex) buildQueryAndCheckIfSargable(field string,
	query, options value.Value, opaque interface{}) *sargableRV {
	rv := &sargableRV{}
	var ok bool
	rv.opaque, ok = opaque.(map[string]interface{})
	if !ok {
		rv.opaque = make(map[string]interface{})
	}

	var err error
	var queryFields map[util.SearchField]struct{}
	var sr *bleve.SearchRequest

	if queryFieldsInterface, exists := rv.opaque["query_fields"]; !exists {
		// if opaque didn't carry a "query" entry, go ahead and
		// process the field+query provided to retrieve queryFields.
		queryFields, sr, err = util.ParseQueryToSearchRequest(field, query)
		if err != nil {
			rv.err = util.N1QLError(err, "failed to parse query to search request")
			return rv
		}

		// update opaqueMap with query, search_request
		rv.opaque["query_fields"] = queryFields
		rv.opaque["search_request"] = sr
	} else {
		queryFields, _ = queryFieldsInterface.(map[util.SearchField]struct{})

		// if an entry for "query" exists, we can assume that an entry for
		// "search_request" also exists.
		srInterface, _ := rv.opaque["search_request"]
		sr, _ = srInterface.(*bleve.SearchRequest)
	}

	rv.searchRequest = sr

	if options != nil {
		// check if an "index" entry exists and if it matches
		indexVal, exists := options.Field("index")
		if exists {
			if indexVal.Type() == value.OBJECT {
				var im *mapping.IndexMappingImpl
				// check if opaque carries an "index" entry
				if imInterface, exists := rv.opaque["index_mapping"]; !exists {
					// if in case this value were an object, it is expected to be
					// a mapping, check if this mapping is compatible with the
					// current index's mapping.
					im, err = util.ConvertValObjectToIndexMapping(indexVal)
					if err != nil {
						rv.err = util.N1QLError(err, "index mapping option isn't valid")
						return rv
					}

					// update opaqueMap
					rv.opaque["index_mapping"] = im
				} else {
					im, _ = imInterface.(*mapping.IndexMappingImpl)
				}

				searchableFields, _, _, dynamic, _, defaultAnalyzer, defaultDateTimeParser :=
					util.ProcessIndexMapping(im)

				if !dynamic && !i.dynamic {
					for k, expect := range searchableFields {
						if got, exists := i.searchableFields[k]; !exists || got != expect {
							return rv
						}
					}

					if (defaultAnalyzer != "" && defaultAnalyzer != i.defaultAnalyzer) ||
						(defaultDateTimeParser != "" &&
							defaultDateTimeParser != i.defaultDateTimeParser) {
						return rv
					}
				}
			} else if indexVal.Type() == value.STRING {
				// if an index name has been provided, check if the current index
				// shares the same name; if not this index is not sargable, also
				// check for indexUUID if available.
				if i.Name() != indexVal.Actual().(string) {
					// not sargable
					return rv
				}
			}
		}

		// check if an "indexUUID" entry exists and if it matches
		indexUUIDVal, indexUUIDAvailable := options.Field("indexUUID")
		if indexUUIDAvailable && indexUUIDVal.Type() == value.STRING {
			if i.Id() != indexUUIDVal.Actual().(string) {
				// not sargable
				return rv
			}
		}
	}

	if i.dynamic {
		// sargable, only if all query fields' analyzers are the same
		// as default analyzer.
		compatibleWithDynamicMapping := true
		for f := range queryFields {
			if f.Analyzer != "" &&
				f.Analyzer != i.defaultAnalyzer {
				compatibleWithDynamicMapping = false
				break
			}
		}
		if compatibleWithDynamicMapping {
			rv.count = len(queryFields)
			if rv.count == 0 {
				// if field(s) not provided or unavailable within query,
				// search is applicable on all indexed fields.
				rv.count = int(i.indexedCount)
			}
			rv.indexedCount = i.indexedCount
			return rv
		}
	}

	isParentFieldSearchable := func(field util.SearchField) bool {
		// check if a prefix of this field name is searchable.
		// - (prefix being delimited by ".")
		// e.g.: potential candidates for "reviews.review.content.author" are:
		// - reviews
		// - reviews.review
		// - reviews.review.content
		// .. only if any of the above mappings are dynamic.
		fieldSplitAtDot := strings.Split(field.Name, ".")
		if len(fieldSplitAtDot) <= 1 {
			// not sargable
			return false
		}

		var matched bool
		entry := fieldSplitAtDot[0]
		for k := 1; k < len(fieldSplitAtDot); k++ {
			searchField := util.SearchField{
				Name:     entry,
				Analyzer: field.Analyzer,
			}
			if dynamic, exists := i.searchableFields[searchField]; exists {
				if dynamic {
					matched = true
					break
				}
			}

			entry += "." + fieldSplitAtDot[k]
		}

		if !matched {
			// not sargable
			return false
		}

		return true
	}

	for f := range queryFields {
		if f.Name == "" {
			// field name not provided/available
			// check if index supports _all field, if not, this query is not sargable
			if !i.allFieldSearchable {
				return rv
			}

			// move on to next query field
			continue
		}

		if f.Type == "" {
			// type isn't available, likely because query value wasn't available;
			// check field name against all possible types
			for _, typ := range []string{
				"boolean", "number", "text", "datetime", "geopoint"} {
				f.Type = typ
				if typ == "text" {
					f.Analyzer = i.defaultAnalyzer
				} else if typ == "datetime" {
					f.DateFormat = i.defaultDateTimeParser
				}

				dynamic, exists := i.searchableFields[f]
				if exists && dynamic {
					// not sargable
				} else if exists {
					break
				} else if !exists {
					if isParentFieldSearchable(f) {
						break
					}
					// else not sargable
				}
				f.Type = ""
				f.Analyzer = ""
				f.DateFormat = ""
			}

			if f.Type == "" {
				// not sargable
				return rv
			}

		} else {
			if f.Type == "text" && f.Analyzer == "" {
				// set analyzer to defaultAnalyzer for those query fields of type:text,
				// that don't have an explicit analyzer set already.
				f.Analyzer = i.defaultAnalyzer
				f.DateFormat = ""
			} else if f.Type == "datetime" && f.DateFormat == "" {
				f.Analyzer = ""
				f.DateFormat = i.defaultDateTimeParser
			}

			dynamic, exists := i.searchableFields[f]
			if exists && dynamic {
				// if searched field contains nested fields, then this field is not
				// searchable, and the query not sargable.
				return rv
			}

			if !exists {
				if !isParentFieldSearchable(f) {
					// not sargable
					return rv
				}
			}
		}
	}

	rv.count = len(queryFields)
	if rv.count == 0 {
		// if field(s) not provided or unavailable within query,
		// index is not sargable if it does not support _all field
		if !i.allFieldSearchable {
			return rv
		}

		// search is applicable on all indexed fields.
		rv.count = int(i.indexedCount)
	}

	// sargable
	rv.indexedCount = i.indexedCount
	return rv
}

// -----------------------------------------------------------------------------

// Pageable returns `true` when it can deliver sorted paged results
// for the requested parameters, and the options are consistent
// across the order[] and the query parameters.
func (i *FTSIndex) Pageable(order []string, offset, limit int64, query,
	options expression.Expression) bool {
	rv := i.pageable(order, offset, limit, query, options)

	if util.Debug > 0 {
		logging.Infof("n1fty: Pageable, index: %s, order: %v,"+
			" offset: %v, limit: %v, query: %v, options: %v, rv: %t",
			i.indexDef.Name, order, offset, limit, query, options, rv)
	}

	return rv
}

func (i *FTSIndex) pageable(order []string, offset, limit int64, query,
	options expression.Expression) bool {
	var queryVal value.Value
	if query != nil {
		queryVal = query.Value()
	}

	// if query contains a searchRequest with some valid pagination
	// info(From, Size or Sort details) then returns false.
	if queryVal != nil {
		if qf, ok := queryVal.Field("query"); ok && qf.Type() == value.OBJECT {
			if util.CheckForPagination(queryVal) {
				// User provided pagination details that could possibly
				// conflict with higher offset/limit settings
				return false
			}
		}
	}

	return offset+limit <= util.GetBleveMaxResultWindow()
}
