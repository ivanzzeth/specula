/**
 * Per-protocol configuration for the cache browser.
 *
 * The API returns uniform CacheEntryDTO fields across all protocols; what
 * differs per protocol is the semantic label for those fields and the
 * human-readable context for trust tiers, empty states, and usage hints.
 */

import type { CacheSort } from '@/api/types';

export interface ProtocolMeta {
  /** Tab / heading label. */
  label: string;
  /** Column header for the `name` field. */
  nameCol: string;
  /** Column header for the `version` field. */
  versionCol: string;
  /** Empty-state message when nothing is cached for this protocol. */
  emptyMsg: string;
  /**
   * One-line explanation of what the tier field means for this protocol.
   * Shown below the tier badge in the detail view.
   */
  tierContext: string;
}

export const PROTOCOLS = [
  'oci',
  'pypi',
  'npm',
  'go',
  'apt',
  'helm',
  'git',
  'tarball',
] as const;

export type ProtocolSlug = (typeof PROTOCOLS)[number];

export function isValidProtocol(s: string): s is ProtocolSlug {
  return (PROTOCOLS as readonly string[]).includes(s);
}

export const PROTOCOL_META: Record<ProtocolSlug, ProtocolMeta> = {
  oci: {
    label: 'OCI',
    nameCol: 'Image',
    versionCol: 'Tag',
    tierContext:
      'cosign keyed verification → signed · first-use digest lock → tofu · no cosign config → checksum',
    emptyMsg:
      'No OCI images cached yet. Pull an image through the proxy to populate this view.',
  },
  pypi: {
    label: 'PyPI',
    nameCol: 'Package',
    versionCol: 'Version',
    tierContext:
      'cross-mirror sha256 quorum (PEP 503 simple-index) → consensus · first-use digest lock → tofu',
    emptyMsg:
      'No PyPI packages cached yet. Set the proxy as your pip index and run pip install.',
  },
  npm: {
    label: 'npm',
    nameCol: 'Package',
    versionCol: 'Version',
    tierContext:
      'registry tarball sha integrity quorum → consensus · first-use digest lock → tofu',
    emptyMsg:
      'No npm packages cached yet. Point npm/yarn/pnpm at the proxy registry and install a package.',
  },
  go: {
    label: 'Go',
    nameCol: 'Module',
    versionCol: 'File',
    tierContext:
      'sumdb Ed25519 Merkle proof (tree head + inclusion proof) → signed · absent sumdb → tofu',
    emptyMsg:
      'No Go modules cached yet. Set GOPROXY to the proxy address and run go get.',
  },
  apt: {
    label: 'apt',
    nameCol: 'Suite / Component',
    versionCol: 'File',
    tierContext:
      'distro keyring GPG over InRelease → signed · pool .deb files → checksum (no per-file GPG)',
    emptyMsg:
      'No apt packages cached yet. Configure /etc/apt/sources.list to use the proxy.',
  },
  helm: {
    label: 'Helm',
    nameCol: 'Chart',
    versionCol: 'Version',
    tierContext:
      '.prov GPG provenance file verified against keyring → signed · no .prov → tofu',
    emptyMsg:
      'No Helm charts cached yet. Add the proxy as a Helm repo and run helm pull.',
  },
  git: {
    label: 'git',
    nameCol: 'Repository',
    versionCol: 'Ref / Object',
    tierContext:
      'signed tag/commit (allowed-signers) → signed · first-clone object hash lock → tofu · git SHA detects force-push history rewrites',
    emptyMsg:
      'No git repositories mirrored yet. Clone through the proxy to build the local mirror.',
  },
  tarball: {
    label: 'tarball',
    nameCol: 'URL',
    versionCol: 'Digest',
    tierContext:
      'cross-mirror digest quorum → consensus · first-use digest lock → tofu',
    emptyMsg: 'No tarballs cached yet.',
  },
};

/** Sort columns with display labels. */
export const SORT_OPTIONS: { value: CacheSort; label: string }[] = [
  { value: 'created_at', label: 'First cached' },
  { value: 'size', label: 'Size' },
  { value: 'name', label: 'Name' },
  { value: 'verified_at', label: 'Verified' },
];
