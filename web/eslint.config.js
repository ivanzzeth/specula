// Flat ESLint config — deliberately narrow.
//
// This exists for ONE class of bug that neither `tsc -b` nor `vite build` can
// see: React's rules of hooks. A hook called after an early return type-checks
// and builds cleanly, then corrupts state at runtime when the render path
// changes. We shipped exactly that (useRegistryHost below three early returns in
// RepoDetail) and it survived every gate — it was caught by a human reading the
// file, which is not a gate.
//
// Style/formatting rules are intentionally absent: they are noise, and this
// codebase has a design system, not a lint-shaped opinion.
import js from '@eslint/js';
import globals from 'globals';
import reactHooks from 'eslint-plugin-react-hooks';
import tseslint from 'typescript-eslint';

export default tseslint.config(
  { ignores: ['dist/**', 'node_modules/**', 'scripts/**'] },
  {
    files: ['src/**/*.{ts,tsx}'],
    extends: [js.configs.recommended, ...tseslint.configs.recommended],
    languageOptions: {
      ecmaVersion: 2022,
      globals: globals.browser,
    },
    plugins: { 'react-hooks': reactHooks },
    rules: {
      // The reason this config exists.
      'react-hooks/rules-of-hooks': 'error',
      'react-hooks/exhaustive-deps': 'warn',
      // Ours are deliberate (catch blocks that degrade silently, etc.).
      '@typescript-eslint/no-unused-vars': ['error', { argsIgnorePattern: '^_' }],
      '@typescript-eslint/no-empty-function': 'off',
    },
  }
);
