// Copyright 2023 The go-commons Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package indexers

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"time"

	elasticsearch "github.com/elastic/go-elasticsearch/v7"
	"github.com/elastic/go-elasticsearch/v7/esutil"
)

const elastic = "elastic"

// Elastic ElasticSearch instance
type Elastic struct {
	index string
}

// ESClient elasticsearch client instance
var ESClient *elasticsearch.Client

// Init function
func init() {
	indexerMap[elastic] = &Elastic{}
}

// Returns new indexer for elastic search
func (esIndexer *Elastic) new(indexerConfig IndexerConfig) error {
	var err error
	if indexerConfig.Index == "" {
		return fmt.Errorf("index name not specified")
	}
	esIndex := strings.ToLower(indexerConfig.Index)
	cfg := elasticsearch.Config{
		Addresses: indexerConfig.Servers,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: indexerConfig.InsecureSkipVerify}},
	}
	ESClient, err = elasticsearch.NewClient(cfg)
	if err != nil {
		return fmt.Errorf("error creating the ES client: %s", err)
	}
	r, err := ESClient.Cluster.Health()
	if err != nil {
		return fmt.Errorf("ES health check failed: %s", err)
	}
	if r.StatusCode != 200 {
		return fmt.Errorf("unexpected ES status code: %d", r.StatusCode)
	}
	esIndexer.index = esIndex
	r, _ = ESClient.Indices.Exists([]string{esIndex})
	if r.IsError() {
		r, _ = ESClient.Indices.Create(esIndex)
		if r.IsError() {
			return fmt.Errorf("error creating index %s on ES: %s", esIndex, r.String())
		}
	}
	return nil
}

// Index uses bulkIndexer to index the documents in the given index
func (esIndexer *Elastic) Index(documents []interface{}, opts IndexingOpts) (string, error) {
	var statString string
	var indexerStatsLock sync.Mutex
	indexerStats := make(map[string]int)

	if len(documents) <= 0 {
		return fmt.Sprintf("Indexing skipped due to %v docs", len(documents)), nil
	}
	hasher := sha256.New()
	bi, err := esutil.NewBulkIndexer(esutil.BulkIndexerConfig{
		Client:     ESClient,
		Index:      esIndexer.index,
		FlushBytes: 5e+6,
		NumWorkers: runtime.NumCPU(),
		Timeout:    10 * time.Minute, // TODO: hardcoded
	})
	if err != nil {
		return "", fmt.Errorf("Error creating the indexer: %s", err)
	}
	start := time.Now().UTC()
	docHash := make(map[string]bool)
	redundantSkipped := 0
	for _, document := range documents {
		j, err := json.Marshal(document)
		if err != nil {
			return "", fmt.Errorf("Cannot encode document %s: %s", document, err)
		}
		hasher.Write(j)
		docId := hex.EncodeToString(hasher.Sum(nil))
		if _, exists := docHash[docId]; exists {
			redundantSkipped += 1
			continue
		}
		err = bi.Add(
			context.Background(),
			esutil.BulkIndexerItem{
				Action:     "index",
				Body:       bytes.NewReader(j),
				DocumentID: docId,
				OnSuccess: func(c context.Context, bii esutil.BulkIndexerItem, biri esutil.BulkIndexerResponseItem) {
					indexerStatsLock.Lock()
					defer indexerStatsLock.Unlock()
					indexerStats[biri.Result]++
				},
			},
		)
		if err != nil {
			return "", fmt.Errorf("Unexpected ES indexing error: %s", err)
		}
		docHash[docId] = true
		hasher.Reset()
	}
	if err := bi.Close(context.Background()); err != nil {
		return "", fmt.Errorf("Unexpected ES error: %s", err)
	}
	dur := time.Since(start)
	for stat, val := range indexerStats {
		statString += fmt.Sprintf(" %s=%d", stat, val)
	}
	if(redundantSkipped > 0){
		statString += fmt.Sprintf(" redundantskipped=%d", redundantSkipped)
	}
	return fmt.Sprintf("Indexing finished in %v:%v", dur.Truncate(time.Millisecond), statString), nil
}
