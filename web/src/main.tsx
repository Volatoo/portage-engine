// First, and out of alphabetical order on purpose. A bundler emits a
// stylesheet where its import is evaluated, and `./app/App` reaches the route
// table, which statically imports all fourteen pages and every
// `styles/pages/*.css` they import — so with the shared sheets imported after
// it, every page rule landed FIRST in the emitted CSS and lost every
// equal-specificity tie to the layer it is written on top of. src/styles/
// cascade.test.ts reads the built bundle and holds this line here.
import './styles/index.css';

import { StrictMode } from 'react';
import { createRoot } from 'react-dom/client';

import { App } from './app/App';
import { ConsoleErrorBoundary } from './app/fallbacks';
import { readBootPayload } from './boot';

// Synchronous, before the first render. The server already resolved the
// language, the auth shape and whatever the URL said about the route; reading
// them here costs nothing, and fetching them instead would cost a frame in the
// wrong state.
const boot = readBootPayload();

const container = document.getElementById('root');
if (container === null) {
  throw new Error('console: the shell rendered without #root');
}

// The outermost boundary, and the only thing between a throw in the providers,
// the router or the chrome itself and an empty <div id="root"> — which is a
// white page an operator cannot tell from an outage, a bad deploy or a typo.
// App wraps every page in a second one, so a page that throws costs the page and
// not the navigation; this is what catches everything above that.
createRoot(container).render(
  <StrictMode>
    <ConsoleErrorBoundary>
      <App boot={boot} />
    </ConsoleErrorBoundary>
  </StrictMode>,
);
