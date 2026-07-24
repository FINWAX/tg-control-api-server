import './globals.css';
import type { ReactNode } from 'react';

export const metadata = {
  title: 'TG Control API Console',
  description: 'Management console for Telegram Control API Server',
};

export default function RootLayout({ children }: { children: ReactNode }) {
  return (
    <html lang="ru">
      <body>{children}</body>
    </html>
  );
}
