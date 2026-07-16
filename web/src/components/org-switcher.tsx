import { useState } from 'react';
import { Plus } from 'lucide-react';

import { CreateOrgDialog } from '@/components/create-org-dialog';
import { useOrg } from '@/components/org-context';
import {
  Select,
  SelectContent,
  SelectItem,
  SelectSeparator,
  SelectTrigger,
  SelectValue,
} from '@/components/ui/select';

/**
 * OrgSwitcher — the multi-tenant context control in the identity rail.
 *
 * Switching orgs re-points the API client's X-Org-Id and persists the choice,
 * so every org-context request that follows is scoped to the new tenant.
 *
 * The org slug is shown rather than its display name: the slug is the registry
 * namespace that appears in a real pull reference
 * (`specula.local/<slug>/<repo>:<tag>`), so it is the identifier an operator
 * needs to recognise. The role sits beside it because what you may do here
 * depends on it, and it changes as you switch.
 */
export function OrgSwitcher() {
  const { orgs, activeOrg, role, loading, switchOrg, refresh } = useOrg();
  const [createOpen, setCreateOpen] = useState(false);

  if (loading) return null;

  // A created org is entered immediately: creating one and then being left in
  // the state that prompted you to create it would be a dead end with an extra
  // step. refresh() re-reads the membership list the switcher renders from.
  function handleCreated(orgId: string) {
    refresh();
    switchOrg(orgId);
  }

  // Belonging to no org is an expected state — only the first user auto-joins
  // the default org; everyone else joins by invitation — but it is never an
  // empty one. Every org-scoped page will reject this account, so name the
  // reason in the rail where the org belongs, and offer the way out (create
  // your own) rather than leaving the user with only a dead chip.
  if (orgs.length === 0) {
    return (
      <>
        <div className="flex items-center gap-2">
          <span
            className="text-micro uppercase tracking-wider text-health-blocked"
            title="You are not a member of any organization. Organizations are invitation-only — ask an administrator to invite this email, or create your own."
          >
            no org — invite required
          </span>
          <button
            type="button"
            onClick={() => setCreateOpen(true)}
            className="text-micro uppercase tracking-wider text-slate-400 underline-offset-2 transition-colors hover:text-brand focus-visible:text-brand focus-visible:outline-none focus-visible:underline"
          >
            create one
          </button>
        </div>
        <CreateOrgDialog
          open={createOpen}
          onOpenChange={setCreateOpen}
          onCreated={(o) => handleCreated(o.id)}
        />
      </>
    );
  }

  return (
    <div className="flex items-center gap-1.5">
      <Select
        value={activeOrg?.id ?? undefined}
        onValueChange={(v) => {
          if (v === CREATE_VALUE) {
            setCreateOpen(true);
            return;
          }
          switchOrg(v);
        }}
      >
        <SelectTrigger
          className="h-6 w-auto min-w-[7rem] gap-2 border-slate-800 bg-transparent px-1.5"
          aria-label="Active organization"
        >
          <SelectValue placeholder="Select org" />
        </SelectTrigger>
        <SelectContent>
          {orgs.map((o) => (
            <SelectItem key={o.id} value={o.id}>
              {o.slug}
            </SelectItem>
          ))}
          <SelectSeparator />
          <SelectItem value={CREATE_VALUE}>
            <span className="flex items-center gap-1.5 text-slate-400">
              <Plus aria-hidden className="size-3" />
              new organization
            </span>
          </SelectItem>
        </SelectContent>
      </Select>

      {role && (
        <span
          className="text-micro uppercase tracking-wider text-slate-500"
          title={`Your role in ${activeOrg?.slug ?? 'this org'}.`}
        >
          {role}
        </span>
      )}

      <CreateOrgDialog
        open={createOpen}
        onOpenChange={setCreateOpen}
        onCreated={(o) => handleCreated(o.id)}
      />
    </div>
  );
}

/**
 * Sentinel for the "new organization" row. Select needs every item to carry a
 * value, and no org id can collide with it.
 */
const CREATE_VALUE = '__create_org__';
