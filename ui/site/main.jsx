import { StrictMode } from 'react';
import { createRoot } from 'react-dom/client';
import '@fontsource-variable/inter';
import '@fontsource-variable/jetbrains-mono';
import '../src/styles.css';
import './site.css';
import Site from './Site.jsx';

createRoot(document.getElementById('root')).render(
  <StrictMode>
    <Site />
  </StrictMode>
);
