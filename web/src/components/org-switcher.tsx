import { useOrg } from '@/components/org-context';
import {
  Select,
  SelectContent,
  SelectItem,
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
  const { orgs, activeOrg, role, loading, switchOrg } = useOrg();

  // Nothing to switch between: stay silent rather than render a dead control.
  if (loading || orgs.length === 0) return null;

  return (
    <div className="flex items-center gap-1.5">
      <Select value={activeOrg?.id ?? undefined} onValueChange={switchOrg}>
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
    </div>
  );
}
