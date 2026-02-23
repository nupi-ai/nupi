package store

import (
	"database/sql"
	"fmt"
)

type rowScanner interface {
	Scan(dest ...any) error
}

func scanAdapter(scanner rowScanner) (Adapter, error) {
	var adapter Adapter
	err := scanner.Scan(
		&adapter.ID,
		&adapter.Source,
		&adapter.Version,
		&adapter.Type,
		&adapter.Name,
		&adapter.Manifest,
		&adapter.CreatedAt,
		&adapter.UpdatedAt,
	)
	return adapter, err
}

func scanAdapterBindingWithSlot(scanner rowScanner) (AdapterBinding, error) {
	var (
		slot      string
		adapterID sql.NullString
		config    sql.NullString
		status    string
		updatedAt string
	)
	if err := scanner.Scan(&slot, &adapterID, &config, &status, &updatedAt); err != nil {
		return AdapterBinding{}, err
	}

	binding := AdapterBinding{
		Slot:      slot,
		Status:    status,
		UpdatedAt: updatedAt,
	}
	if adapterID.Valid {
		binding.AdapterID = &adapterID.String
	}
	if config.Valid {
		binding.Config = config.String
	}
	return binding, nil
}

func scanAdapterEndpoint(scanner rowScanner) (AdapterEndpoint, error) {
	var (
		argsRaw     sql.NullString
		envRaw      sql.NullString
		tlsInsecure int
		endpoint    AdapterEndpoint
	)
	if err := scanner.Scan(
		&endpoint.AdapterID,
		&endpoint.Transport,
		&endpoint.Address,
		&endpoint.Command,
		&argsRaw,
		&envRaw,
		&endpoint.TLSCertPath,
		&endpoint.TLSKeyPath,
		&endpoint.TLSCACertPath,
		&tlsInsecure,
		&endpoint.CreatedAt,
		&endpoint.UpdatedAt,
	); err != nil {
		return AdapterEndpoint{}, err
	}
	endpoint.TLSInsecure = tlsInsecure != 0

	args, err := DecodeJSON[[]string](argsRaw)
	if err != nil {
		return AdapterEndpoint{}, fmt.Errorf("decode adapter endpoint args for %s: %w", endpoint.AdapterID, err)
	}
	endpoint.Args = args

	env, err := DecodeJSON[map[string]string](envRaw)
	if err != nil {
		return AdapterEndpoint{}, fmt.Errorf("decode adapter endpoint env for %s: %w", endpoint.AdapterID, err)
	}
	endpoint.Env = env
	return endpoint, nil
}

func scanProfile(scanner rowScanner) (Profile, error) {
	var (
		name      string
		isDefault int
		createdAt string
		updatedAt string
	)
	if err := scanner.Scan(&name, &isDefault, &createdAt, &updatedAt); err != nil {
		return Profile{}, err
	}
	return Profile{
		Name:      name,
		IsDefault: isDefault == 1,
		CreatedAt: createdAt,
		UpdatedAt: updatedAt,
	}, nil
}

func scanString(scanner rowScanner) (string, error) {
	var value string
	err := scanner.Scan(&value)
	return value, err
}

func scanStringPair(scanner rowScanner) (string, string, error) {
	var key, value string
	err := scanner.Scan(&key, &value)
	return key, value, err
}

func scanMarketplace(scanner rowScanner) (Marketplace, error) {
	var (
		marketplace                Marketplace
		cachedIndex, lastRefreshed sql.NullString
	)
	err := scanner.Scan(
		&marketplace.ID,
		&marketplace.InstanceName,
		&marketplace.Namespace,
		&marketplace.URL,
		&marketplace.IsBuiltin,
		&cachedIndex,
		&lastRefreshed,
		&marketplace.CreatedAt,
	)
	if err != nil {
		return Marketplace{}, err
	}
	marketplace.CachedIndex = cachedIndex.String
	marketplace.LastRefreshed = lastRefreshed.String
	return marketplace, nil
}

func scanInstalledPluginWithNamespace(scanner rowScanner) (InstalledPluginWithNamespace, error) {
	var (
		plugin    InstalledPluginWithNamespace
		sourceURL sql.NullString
	)
	err := scanner.Scan(
		&plugin.ID,
		&plugin.MarketplaceID,
		&plugin.Slug,
		&sourceURL,
		&plugin.InstalledAt,
		&plugin.Enabled,
		&plugin.Namespace,
	)
	if err != nil {
		return InstalledPluginWithNamespace{}, err
	}
	plugin.SourceURL = sourceURL.String
	return plugin, nil
}

func scanPromptTemplate(scanner rowScanner) (PromptTemplate, error) {
	var (
		template PromptTemplate
		isCustom int
	)
	err := scanner.Scan(&template.EventType, &template.Content, &isCustom, &template.UpdatedAt)
	if err != nil {
		return PromptTemplate{}, err
	}
	template.IsCustom = isCustom != 0
	return template, nil
}

func scanPluginChecksum(scanner rowScanner) (PluginChecksum, error) {
	var checksum PluginChecksum
	err := scanner.Scan(&checksum.PluginID, &checksum.FilePath, &checksum.SHA256, &checksum.CreatedAt)
	return checksum, err
}
