import { Api, type Schemas } from '../core/api';

type Member = Schemas['Member'];

const PAGE = 50;

/** Every registered member. The members list shows one page at a time; counts and lookups need all of them. */
export async function fetchAllMembers(client: Api['client']): Promise<Member[]> {
  const rows: Member[] = [];
  let after: string | undefined;
  for (;;) {
    const page = Api.ok(
      await client.GET('/v1/admin/members', { params: { query: { after, limit: PAGE } } }),
    );
    rows.push(...page);
    if (page.length < PAGE) return rows;
    after = page.at(-1)?.id;
  }
}
