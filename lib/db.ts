import { Pool, type QueryResult, type QueryResultRow } from "pg";
import { env } from "./env";

const globalForDb = globalThis as unknown as { __pgPool?: Pool };

export function pool(): Pool {
  if (!globalForDb.__pgPool) {
    globalForDb.__pgPool = new Pool({
      connectionString: env.databaseUrl,
      max: 10,
      idleTimeoutMillis: 30_000,
    });
  }
  return globalForDb.__pgPool;
}

export function query<T extends QueryResultRow = QueryResultRow>(
  text: string,
  params: unknown[] = [],
): Promise<QueryResult<T>> {
  return pool().query<T>(text, params);
}
