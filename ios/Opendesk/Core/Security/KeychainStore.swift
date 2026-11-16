import Foundation
import Security

/// Generic-password Keychain items for this app: the signed-in accounts and
/// their refresh tokens (one item per account), and App Attest key ids (one per
/// server). Readable after first unlock (push wakes and background refresh) and
/// never synced or backed up to another device.
enum KeychainStore {
    private static let service = "sh.opendesk.app"
    private static let installMarker = "keychain.install"

    /// iOS keeps an app's Keychain items after the app is deleted, while its UserDefaults go
    /// with it. A launch with no install marker in UserDefaults removes the items a deleted
    /// install left behind, so a reinstall (or a device handed to someone else) never signs
    /// anyone back in. An install that already has settings, from a version before the
    /// marker, keeps its items. Call it before anything reads the Keychain.
    static func forgetPreviousInstall() {
        #if DEBUG
        if DemoMode.isOn { return }
        #endif
        let defaults = UserDefaults.standard
        guard !defaults.bool(forKey: installMarker) else { return }
        let hadSettings = Bundle.main.bundleIdentifier
            .flatMap { defaults.persistentDomain(forName: $0) }
            .map { !$0.isEmpty } ?? false
        if !hadSettings {
            let all: [String: Any] = [
                kSecClass as String: kSecClassGenericPassword,
                kSecAttrService as String: service,
            ]
            SecItemDelete(all as CFDictionary)
        }
        defaults.set(true, forKey: installMarker)
    }

    /// Stores `value`; false when the Keychain refused it (the caller's session then lasts
    /// only until the app quits).
    @discardableResult
    static func set(_ value: String, for account: String) -> Bool {
        #if DEBUG
        if DemoMode.isOn {
            DemoMode.setKeychainValue(value, for: account)
            return true
        }
        #endif
        let data = Data(value.utf8)
        let query = baseQuery(account)
        let attributes: [String: Any] = [
            kSecValueData as String: data,
            kSecAttrAccessible as String: kSecAttrAccessibleAfterFirstUnlockThisDeviceOnly,
        ]
        var status = SecItemUpdate(query as CFDictionary, attributes as CFDictionary)
        if status == errSecItemNotFound {
            var insert = query
            insert[kSecValueData as String] = data
            insert[kSecAttrAccessible as String] = kSecAttrAccessibleAfterFirstUnlockThisDeviceOnly
            status = SecItemAdd(insert as CFDictionary, nil)
        }
        return status == errSecSuccess
    }

    static func get(_ account: String) -> String? {
        #if DEBUG
        if DemoMode.isOn { return DemoMode.keychainValue(account) }
        #endif
        var query = baseQuery(account)
        query[kSecReturnData as String] = true
        query[kSecMatchLimit as String] = kSecMatchLimitOne
        var result: AnyObject?
        guard SecItemCopyMatching(query as CFDictionary, &result) == errSecSuccess,
              let data = result as? Data
        else { return nil }
        return String(data: data, encoding: .utf8)
    }

    static func remove(_ account: String) {
        #if DEBUG
        if DemoMode.isOn { return DemoMode.setKeychainValue(nil, for: account) }
        #endif
        SecItemDelete(baseQuery(account) as CFDictionary)
    }

    private static func baseQuery(_ account: String) -> [String: Any] {
        [
            kSecClass as String: kSecClassGenericPassword,
            kSecAttrService as String: service,
            kSecAttrAccount as String: account,
        ]
    }
}
