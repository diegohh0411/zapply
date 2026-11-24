// A real TanStack QueryClient with a small stand-in for injectQuery, so tests
// can run production classes that keep their server state in the query cache.
const { QueryClient, QueryObserver } = require('@tanstack/query-core');

class QueryClientToken {}

/**
 * `signal` is the test's signal factory: with real Angular signals, reads of a
 * query are reactive (the observer bumps a version when its result changes).
 */
function queryHarness(signal = (v) => Object.assign(() => v, { set: (n) => (v = n) })) {
  // gcTime: Infinity keeps query-core from leaving timers that hold the process open.
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false, gcTime: Infinity } },
  });
  const injectQuery = (optionsFn) => {
    const version = signal(0);
    let observer;
    const current = () => {
      version();
      const options = client.defaultQueryOptions(optionsFn());
      if (!observer) {
        observer = new QueryObserver(client, options);
        // Reads can happen inside a computed, where Angular forbids signal
        // writes, so the change is announced after the current read.
        observer.subscribe(() => queueMicrotask(() => version.set(version() + 1)));
      } else observer.setOptions(options);
      return observer.getOptimisticResult(options);
    };
    return new Proxy({}, { get: (_, key) => () => current()[key] });
  };
  return {
    client,
    tanstack: { QueryClient: QueryClientToken, injectQuery },
    QueryClientToken,
  };
}

module.exports = { queryHarness };
