/**
 * The messages object, in React.
 *
 * Plain `createElement` rather than JSX so this stays a `.ts` module: a file
 * that exports a component and a hook together is a fast-refresh boundary the
 * bundler cannot reason about, and the hook is the export every component wants.
 */

import { createContext, createElement, useContext } from 'react';
import type { ReactNode } from 'react';

import { messagesFor } from './messages';
import type { Language, Messages } from './messages';

/**
 * Seeded with English rather than left null. A component rendered outside the
 * provider — in a test, or inside an error boundary that replaced the tree — is
 * a component that should still render words.
 */
const MessagesContext = createContext<Messages>(messagesFor('en'));

export function MessagesProvider(props: { lang: Language; children: ReactNode }): ReactNode {
  return createElement(
    MessagesContext.Provider,
    { value: messagesFor(props.lang) },
    props.children,
  );
}

export function useMessages(): Messages {
  return useContext(MessagesContext);
}
