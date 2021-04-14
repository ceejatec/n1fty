module github.com/couchbase/n1fty

go 1.13

replace github.com/couchbase/cbauth => ../cbauth

replace github.com/couchbase/query => ../query

replace github.com/couchbase/n1fty => ./empty

replace github.com/couchbase/query-ee => ../query-ee

replace github.com/couchbase/eventing-ee => ../eventing-ee

replace github.com/couchbase/go-couchbase => ../go-couchbase

replace github.com/couchbase/gomemcached => ../gomemcached

replace github.com/couchbase/go_json => ../go_json

replace github.com/couchbase/indexing => ../indexing

replace github.com/couchbase/plasma => ../plasma

replace github.com/couchbase/cbft => ../../../../../cbft

replace github.com/couchbase/cbftx => ../../../../../cbftx

replace github.com/couchbase/cbgt => ../../../../../cbgt

require (
	github.com/blevesearch/bleve/v2 v2.0.3
	github.com/buger/jsonparser v1.1.1
	github.com/couchbase/cbauth v0.0.0-20201026062450-0eaf917092a2
	github.com/couchbase/cbft v0.4.0
	github.com/couchbase/cbgt v0.0.0-20210412150823-5fbd5c755c7e
	github.com/couchbase/gocbcore/v9 v9.1.4-0.20210325182448-577aecce6dc6
	github.com/couchbase/moss v0.1.0
	github.com/couchbase/query v0.0.0-20210414101450-488417774d57
	google.golang.org/grpc v1.37.0
)
