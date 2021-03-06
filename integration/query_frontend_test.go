// +build requires_docker

package main

import (
	"fmt"
	"os/exec"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/prompb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/cortexproject/cortex/integration/e2e"
	e2ecache "github.com/cortexproject/cortex/integration/e2e/cache"
	e2edb "github.com/cortexproject/cortex/integration/e2e/db"
	"github.com/cortexproject/cortex/integration/e2ecortex"
)

const (
	integrationHomeFolder = "integration/"
)

type queryFrontendSetup func(t *testing.T, s *e2e.Scenario) (configFile string, flags map[string]string)

func TestQueryFrontendWithBlocksStorageViaFlags(t *testing.T) {
	runQueryFrontendTest(t, false, func(t *testing.T, s *e2e.Scenario) (configFile string, flags map[string]string) {
		minio := e2edb.NewMinio(9000, BlocksStorageFlags["-experimental.tsdb.s3.bucket-name"])
		require.NoError(t, s.StartAndWaitReady(minio))

		return "", BlocksStorageFlags
	})
}

func TestQueryFrontendWithBlocksStorageViaConfigFile(t *testing.T) {
	runQueryFrontendTest(t, false, func(t *testing.T, s *e2e.Scenario) (configFile string, flags map[string]string) {
		require.NoError(t, writeFileToSharedDir(s, cortexConfigFile, []byte(BlocksStorageConfig)))

		minio := e2edb.NewMinio(9000, BlocksStorageFlags["-experimental.tsdb.s3.bucket-name"])
		require.NoError(t, s.StartAndWaitReady(minio))

		return cortexConfigFile, e2e.EmptyFlags()
	})
}

func TestQueryFrontendWithChunksStorageViaFlags(t *testing.T) {
	runQueryFrontendTest(t, true, func(t *testing.T, s *e2e.Scenario) (configFile string, flags map[string]string) {
		require.NoError(t, writeFileToSharedDir(s, cortexSchemaConfigFile, []byte(cortexSchemaConfigYaml)))

		dynamo := e2edb.NewDynamoDB()
		require.NoError(t, s.StartAndWaitReady(dynamo))

		tableManager := e2ecortex.NewTableManager("table-manager", ChunksStorageFlags, "")
		require.NoError(t, s.StartAndWaitReady(tableManager))

		// Wait until the first table-manager sync has completed, so that we're
		// sure the tables have been created.
		require.NoError(t, tableManager.WaitSumMetrics(e2e.Greater(0), "cortex_table_manager_sync_success_timestamp_seconds"))

		return "", ChunksStorageFlags
	})
}

func TestQueryFrontendWithChunksStorageViaConfigFile(t *testing.T) {
	runQueryFrontendTest(t, true, func(t *testing.T, s *e2e.Scenario) (configFile string, flags map[string]string) {
		require.NoError(t, writeFileToSharedDir(s, cortexConfigFile, []byte(ChunksStorageConfig)))
		require.NoError(t, writeFileToSharedDir(s, cortexSchemaConfigFile, []byte(cortexSchemaConfigYaml)))

		dynamo := e2edb.NewDynamoDB()
		require.NoError(t, s.StartAndWaitReady(dynamo))

		tableManager := e2ecortex.NewTableManagerWithConfigFile("table-manager", cortexConfigFile, e2e.EmptyFlags(), "")
		require.NoError(t, s.StartAndWaitReady(tableManager))

		// Wait until the first table-manager sync has completed, so that we're
		// sure the tables have been created.
		require.NoError(t, tableManager.WaitSumMetrics(e2e.Greater(0), "cortex_table_manager_sync_success_timestamp_seconds"))

		return cortexConfigFile, e2e.EmptyFlags()
	})
}

func TestQueryFrontendTLSWithBlocksStorageViaFlags(t *testing.T) {
	runQueryFrontendTest(t, false, func(t *testing.T, s *e2e.Scenario) (configFile string, flags map[string]string) {
		minio := e2edb.NewMinio(9000, BlocksStorageFlags["-experimental.tsdb.s3.bucket-name"])
		require.NoError(t, s.StartAndWaitReady(minio))

		// setup tls
		cmd := exec.Command("bash", "certs/genCerts.sh", "certs", "1")
		require.NoError(t, cmd.Run())
		require.NoError(t, copyFileToSharedDir(s, integrationHomeFolder+clientCertFile, clientCertFile))
		require.NoError(t, copyFileToSharedDir(s, integrationHomeFolder+clientKeyFile, clientKeyFile))
		require.NoError(t, copyFileToSharedDir(s, integrationHomeFolder+caCertFile, caCertFile))
		require.NoError(t, copyFileToSharedDir(s, integrationHomeFolder+serverCertFile, serverCertFile))
		require.NoError(t, copyFileToSharedDir(s, integrationHomeFolder+serverKeyFile, serverKeyFile))

		return "", mergeFlags(
			BlocksStorageFlags,
			getServerTLSFlags(),
			getClientTLSFlagsWithPrefix("ingester.client"),
			getClientTLSFlagsWithPrefix("querier.frontend-client"),
			getClientTLSFlagsWithPrefix("ingester.client"),
		)
	})
}

func runQueryFrontendTest(t *testing.T, testMissingMetricName bool, setup queryFrontendSetup) {
	const numUsers = 10
	const numQueriesPerUser = 10

	s, err := e2e.NewScenario(networkName)
	require.NoError(t, err)
	defer s.Close()

	memcached := e2ecache.NewMemcached()
	consul := e2edb.NewConsul()
	require.NoError(t, s.StartAndWaitReady(consul, memcached))

	configFile, flags := setup(t, s)

	flags = mergeFlags(flags, map[string]string{
		"-querier.cache-results":             "true",
		"-querier.split-queries-by-interval": "24h",
		"-frontend.memcached.addresses":      "dns+" + memcached.NetworkEndpoint(e2ecache.MemcachedPort),
	})

	// Start Cortex components.
	queryFrontend := e2ecortex.NewQueryFrontendWithConfigFile("query-frontend", configFile, flags, "")
	ingester := e2ecortex.NewIngesterWithConfigFile("ingester", consul.NetworkHTTPEndpoint(), configFile, flags, "")
	distributor := e2ecortex.NewDistributorWithConfigFile("distributor", consul.NetworkHTTPEndpoint(), configFile, flags, "")

	require.NoError(t, s.Start(queryFrontend))

	querier := e2ecortex.NewQuerierWithConfigFile("querier", consul.NetworkHTTPEndpoint(), configFile, mergeFlags(flags, map[string]string{
		"-querier.frontend-address": queryFrontend.NetworkGRPCEndpoint(),
	}), "")

	require.NoError(t, s.StartAndWaitReady(querier, ingester, distributor))
	require.NoError(t, s.WaitReady(queryFrontend))

	// Check if we're discovering memcache or not.
	require.NoError(t, queryFrontend.WaitSumMetrics(e2e.Equals(1), "cortex_memcache_client_servers"))
	require.NoError(t, queryFrontend.WaitSumMetrics(e2e.Greater(0), "cortex_dns_lookups_total"))

	// Wait until both the distributor and querier have updated the ring.
	require.NoError(t, distributor.WaitSumMetrics(e2e.Equals(512), "cortex_ring_tokens_total"))
	require.NoError(t, querier.WaitSumMetrics(e2e.Equals(512), "cortex_ring_tokens_total"))

	// Push a series for each user to Cortex.
	now := time.Now()
	expectedVectors := make([]model.Vector, numUsers)

	for u := 0; u < numUsers; u++ {
		c, err := e2ecortex.NewClient(distributor.HTTPEndpoint(), "", "", "", fmt.Sprintf("user-%d", u))
		require.NoError(t, err)

		var series []prompb.TimeSeries
		series, expectedVectors[u] = generateSeries("series_1", now)

		res, err := c.Push(series)
		require.NoError(t, err)
		require.Equal(t, 200, res.StatusCode)
	}

	// Query the series for each user in parallel.
	wg := sync.WaitGroup{}
	wg.Add(numUsers * numQueriesPerUser)

	for u := 0; u < numUsers; u++ {
		userID := u

		c, err := e2ecortex.NewClient("", queryFrontend.HTTPEndpoint(), "", "", fmt.Sprintf("user-%d", userID))
		require.NoError(t, err)

		// No need to repeat this test for each user.
		if userID == 0 && testMissingMetricName {
			res, body, err := c.QueryRaw("{instance=~\"hello.*\"}")
			require.NoError(t, err)
			require.Equal(t, 422, res.StatusCode)
			require.Contains(t, string(body), "query must contain metric name")
		}

		for q := 0; q < numQueriesPerUser; q++ {
			go func() {
				defer wg.Done()

				result, err := c.Query("series_1", now)
				require.NoError(t, err)
				require.Equal(t, model.ValVector, result.Type())
				assert.Equal(t, expectedVectors[userID], result.(model.Vector))
			}()
		}
	}

	wg.Wait()

	extra := float64(0)
	if testMissingMetricName {
		extra = 1
	}
	require.NoError(t, queryFrontend.WaitSumMetrics(e2e.Equals(numUsers*numQueriesPerUser+extra), "cortex_query_frontend_queries_total"))

	// Ensure no service-specific metrics prefix is used by the wrong service.
	assertServiceMetricsPrefixes(t, Distributor, distributor)
	assertServiceMetricsPrefixes(t, Ingester, ingester)
	assertServiceMetricsPrefixes(t, Querier, querier)
	assertServiceMetricsPrefixes(t, QueryFrontend, queryFrontend)
}
