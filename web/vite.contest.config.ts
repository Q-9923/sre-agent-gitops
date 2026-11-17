import { fileURLToPath } from 'node:url';
import { defineConfig } from 'vite';
import react from '@vitejs/plugin-react';

export default defineConfig({
  plugins: [react()],
  publicDir: false,
  build: {
    outDir: 'dist-contest',
    rolldownOptions: { input: fileURLToPath(new URL('./contest.html', import.meta.url)) },
  },
});
