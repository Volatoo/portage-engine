import js from '@eslint/js';
import prettier from 'eslint-config-prettier';
import reactHooks from 'eslint-plugin-react-hooks';
import reactRefresh from 'eslint-plugin-react-refresh';
import globals from 'globals';
import tseslint from 'typescript-eslint';

/**
 * The restricted-syntax entries below are the console's own invariants, not
 * style. Each one exists because losing it is a break a reader hits before
 * anyone working on the code does: an escape hatch back to innerHTML, a
 * language read from the operating system, or a theme decided in JavaScript.
 */
const CONSOLE_INVARIANTS = [
  {
    selector: "JSXAttribute[name.name='dangerouslySetInnerHTML']",
    message: 'No markup from API data. React escapes text for you; build nodes, not strings.',
  },
  {
    selector: 'MemberExpression[property.name=/^(innerHTML|outerHTML)$/]',
    message: 'No markup from API data. React escapes text for you; build nodes, not strings.',
  },
  {
    selector: "MemberExpression[object.name='navigator'][property.name=/^language/]",
    message:
      'The server resolves the language and sends it in the boot payload. navigator.language is the operating system locale, which is the leak that server-side resolution exists to prevent.',
  },
  {
    selector:
      "CallExpression[callee.name='matchMedia'], CallExpression[callee.property.name='matchMedia']",
    message:
      'Theme is prefers-color-scheme in CSS, remapping tokens. There is no stored theme and no theme state in JavaScript.',
  },
  {
    selector: "NewExpression[callee.object.name='Intl'][arguments.length<1]",
    message:
      'Pass the app locale to Intl explicitly. With no locale it formats to the operating system one, so a reader who chose English on a zh-CN machine gets Chinese numbers and dates.',
  },
  {
    selector: 'CallExpression[callee.property.name=/^toLocale/][arguments.length=0]',
    message:
      'Pass the app locale explicitly. toLocale* with no argument formats to the operating system locale.',
  },
];

export default tseslint.config(
  { ignores: ['node_modules', '../internal/dashboard/webassets/bundle'] },
  js.configs.recommended,
  {
    files: ['**/*.{ts,tsx}'],
    // Type-aware linting is scoped to the TypeScript sources: the rules it turns
    // on need a program, and the config files below are not in one.
    extends: [tseslint.configs.recommendedTypeChecked],
    languageOptions: {
      ecmaVersion: 2022,
      globals: globals.browser,
      parserOptions: {
        projectService: true,
        tsconfigRootDir: import.meta.dirname,
      },
    },
    rules: {
      'no-restricted-syntax': ['error', ...CONSOLE_INVARIANTS],
      // `window.status`, `window.name` and `window.length` are always in scope,
      // so deleting the local `status` a component destructured leaves the file
      // compiling and the comparison silently reading the DOM's. That is what a
      // `status === null` guard against a removed binding did here: `tsc` stayed
      // green and the branch became unreachable.
      'no-restricted-globals': ['error', 'status', 'name', 'length', 'event', 'origin'],
      '@typescript-eslint/consistent-type-imports': 'error',
      eqeqeq: ['error', 'always', { null: 'ignore' }],
    },
  },
  {
    files: ['**/*.tsx'],
    // The plugin ships an eslintrc-shaped and a flat-shaped copy of the same rule
    // set under similar names; only the one under `flat` is a flat config.
    extends: [reactHooks.configs.flat['recommended-latest']],
    plugins: { 'react-refresh': reactRefresh },
    rules: {
      'react-refresh/only-export-components': ['warn', { allowConstantExport: true }],
    },
  },
  {
    // The tooling config files and the stylelint plugin run in Node and are
    // outside the TypeScript program, so the type-aware rules have nothing to
    // read here.
    files: ['*.config.js', 'stylelint-plugins/*.js'],
    extends: [tseslint.configs.disableTypeChecked],
    languageOptions: { globals: globals.node },
  },
  prettier,
);
