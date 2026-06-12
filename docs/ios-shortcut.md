# iOS Shortcut: capture to recall

Gives you Siri + Share Sheet capture in ~5 minutes, no code. (PLAN.md Phase 3.)

You need two values:
- **API URL**: `https://b229txcv81.execute-api.us-west-2.amazonaws.com`
- **Token**: the value stored in SSM at `/recall/api-token`

## Steps (on your iPhone)

1. Open **Shortcuts** → **+** to create a new shortcut. Name it **Recall**.
2. Add action **Get Text from Input** (so shared text/pages become plain text).
3. Add action **Get Contents of URL** and configure:
   - URL: `https://b229txcv81.execute-api.us-west-2.amazonaws.com/entries`
   - Tap **Show More**:
     - Method: **POST**
     - Headers:
       - `Authorization` → `Bearer <your token>`
       - `Content-Type` → `application/json`
     - Request Body: **JSON**
       - `text` (Text) → the **Text** variable from step 2
       - `source` (Text) → `shortcut`
4. (Optional) Add **Show Notification** with "saved ✓" so you get confirmation.
5. Tap the shortcut's settings (ⓘ):
   - Enable **Show in Share Sheet** (accepts Text, URLs, Safari pages).
   - Under Share Sheet Types, select Text + URLs.
6. Say "Hey Siri, Recall" to test voice capture, or share any text → **Recall**.

## If no input is shared

Add **Ask for Input** (Text) as the first action with prompt "What do you want
to remember?" — then the shortcut works standalone from the home screen too.

## Web capture

The same API serves a capture page at the root URL — open
`https://b229txcv81.execute-api.us-west-2.amazonaws.com/` on any device,
paste the token once (stored in localStorage), and add it to your phone's
home screen for one-tap capture.
