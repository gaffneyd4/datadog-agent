# Unless explicitly stated otherwise all files in this repository are licensed
# under the Apache License Version 2.0.
# This product includes software developed at Datadog (https:#www.datadoghq.com/).
# Copyright 2016-present Datadog, Inc.
require "./lib/ostools.rb"

flavor = ENV['AGENT_FLAVOR']

if flavor.nil? || flavor == 'base'
  name 'agent'
  package_name 'datadog-agent'
else
  name "agent-#{flavor}"
  package_name "datadog-#{flavor}-agent"
end
license "Apache-2.0"
license_file "../LICENSE"

homepage 'http://www.datadoghq.com'

if ohai['platform'] == "windows"
  # Note: this is the path used by Omnibus to build the agent, the final install
  # dir will be determined by the Windows installer. This path must not contain
  # spaces because Omnibus doesn't quote the Git commands it launches.
  install_dir "C:/opt/datadog-agent/"
  python_2_embedded "#{install_dir}/embedded2"
  python_3_embedded "#{install_dir}/embedded3"
  maintainer 'Datadog Inc.' # Windows doesn't want our e-mail address :(
else
  if redhat? || suse?
    maintainer 'Datadog, Inc <package@datadoghq.com>'

    # NOTE: with script dependencies, we only care about preinst/postinst/posttrans,
    # because these would be used in a kickstart during package installation phase.
    # All of the packages that we depend on in prerm/postrm scripts always have to be
    # installed on all distros that we support, so we don't have to depend on them
    # explicitly.

    # postinst and posttrans scripts use a subset of preinst script deps, so we don't
    # have to list them, because they'll already be there because of preinst
    runtime_script_dependency :pre, "coreutils"
    runtime_script_dependency :pre, "findutils"
    runtime_script_dependency :pre, "grep"
    if redhat?
      runtime_script_dependency :pre, "glibc-common"
      runtime_script_dependency :pre, "shadow-utils"
    else
      runtime_script_dependency :pre, "glibc"
      runtime_script_dependency :pre, "shadow"
    end
  else
    maintainer 'Datadog Packages <package@datadoghq.com>'
  end

  if debian?
    runtime_recommended_dependency 'datadog-signing-keys'
  end

  if osx?
    unless ENV['SKIP_SIGN_MAC'] == 'true'
      code_signing_identity 'Developer ID Application: Datadog, Inc. (JKFCB4CN7C)'
    end
    if ENV['HARDENED_RUNTIME_MAC'] == 'true'
      entitlements_file "#{files_path}/macos/Entitlements.plist"
    end
  end

  install_dir '/opt/datadog-agent'
end

# build_version is computed by an invoke command/function.
# We can't call it directly from there, we pass it through the environment instead.
build_version ENV['PACKAGE_VERSION']

build_iteration 1

description 'Datadog Monitoring Agent
 The Datadog Monitoring Agent is a lightweight process that monitors system
 processes and services, and sends information back to your Datadog account.
 .
 This package installs and runs the advanced Agent daemon, which queues and
 forwards metrics from your applications as well as system services.
 .
 See http://www.datadoghq.com/ for more information
'

# ------------------------------------
# Generic package information
# ------------------------------------

# .deb specific flags
package :deb do
  vendor 'Datadog <package@datadoghq.com>'
  epoch 1
  license 'Apache License Version 2.0'
  section 'utils'
  priority 'extra'
  if ENV.has_key?('DEB_SIGNING_PASSPHRASE') and not ENV['DEB_SIGNING_PASSPHRASE'].empty?
    signing_passphrase "#{ENV['DEB_SIGNING_PASSPHRASE']}"
    if ENV.has_key?('DEB_GPG_KEY_NAME') and not ENV['DEB_GPG_KEY_NAME'].empty?
      gpg_key_name "#{ENV['DEB_GPG_KEY_NAME']}"
    end
  end
end

# .rpm specific flags
package :rpm do
  vendor 'Datadog <package@datadoghq.com>'
  epoch 1
  dist_tag ''
  license 'Apache License Version 2.0'
  category 'System Environment/Daemons'
  priority 'extra'
  if ENV.has_key?('RPM_SIGNING_PASSPHRASE') and not ENV['RPM_SIGNING_PASSPHRASE'].empty?
    signing_passphrase "#{ENV['RPM_SIGNING_PASSPHRASE']}"
  end
end

# OSX .pkg specific flags
package :pkg do
  identifier 'com.datadoghq.agent'
  unless ENV['SKIP_SIGN_MAC'] == 'true'
    signing_identity 'Developer ID Installer: Datadog, Inc. (JKFCB4CN7C)'
  end
end
compress :dmg do
  window_bounds '200, 200, 584, 632'

  pkg_position '10, 10'
end

# Windows .zip specific flags
package :zip do
  if windows_arch_i386?
    skip_packager true
  else
    extra_package_dirs [
      "#{Omnibus::Config.source_dir()}\\etc\\datadog-agent\\extra_package_files",
      "#{Omnibus::Config.source_dir()}\\cf-root",
    ]

    # Always sign everything for binaries zip
    additional_sign_files [
        "#{Omnibus::Config.source_dir()}\\cf-root\\bin\\agent\\security-agent.exe",
        "#{Omnibus::Config.source_dir()}\\cf-root\\bin\\agent\\process-agent.exe",
        "#{Omnibus::Config.source_dir()}\\cf-root\\bin\\agent\\trace-agent.exe",
        "#{Omnibus::Config.source_dir()}\\cf-root\\bin\\agent.exe",
        "#{Omnibus::Config.source_dir()}\\cf-root\\bin\\libdatadog-agent-three.dll",
        "#{Omnibus::Config.source_dir()}\\cf-root\\bin\\agent\\install-cmd.exe",
        "#{Omnibus::Config.source_dir()}\\cf-root\\bin\\agent\\uninstall-cmd.exe"
      ]
    if with_python_runtime? "2"
      additional_sign_files << "#{Omnibus::Config.source_dir()}\\cf-root\\bin\\libdatadog-agent-two.dll"
    end
    if ENV['SIGN_PFX']
      signing_identity_file "#{ENV['SIGN_PFX']}", password: "#{ENV['SIGN_PFX_PW']}", algorithm: "SHA256"
    end
  end
end

package :msi do

  # For a consistent package management, please NEVER change this code
  arch = "x64"
  if windows_arch_i386?
    upgrade_code '2497f989-f07e-4e8c-9e05-841ad3d4405f'
    arch = "x86"
  else
    upgrade_code '0c50421b-aefb-4f15-a809-7af256d608a5'
  end
  wix_candle_extension 'WixUtilExtension'
  wix_light_extension 'WixUtilExtension'
  extra_package_dir "#{Omnibus::Config.source_dir()}\\etc\\datadog-agent\\extra_package_files"

  additional_sign_files_list = [
      "#{Omnibus::Config.source_dir()}\\datadog-agent\\src\\github.com\\DataDog\\datadog-agent\\bin\\agent\\security-agent.exe",
      "#{Omnibus::Config.source_dir()}\\datadog-agent\\src\\github.com\\DataDog\\datadog-agent\\bin\\agent\\process-agent.exe",
      "#{Omnibus::Config.source_dir()}\\datadog-agent\\src\\github.com\\DataDog\\datadog-agent\\bin\\agent\\trace-agent.exe",
      "#{Omnibus::Config.source_dir()}\\datadog-agent\\src\\github.com\\DataDog\\datadog-agent\\bin\\agent\\agent.exe",
      "#{Omnibus::Config.source_dir()}\\datadog-agent\\src\\github.com\\DataDog\\datadog-agent\\bin\\agent\\libdatadog-agent-three.dll",
      "#{install_dir}\\bin\\agent\\ddtray.exe"
    ]
    if with_python_runtime? "2"
      additional_sign_files_list << "#{Omnibus::Config.source_dir()}\\datadog-agent\\src\\github.com\\DataDog\\datadog-agent\\bin\\agent\\libdatadog-agent-two.dll"
    end
  #if ENV['SIGN_WINDOWS']
  #  signing_identity "ECCDAE36FDCB654D2CBAB3E8975AA55469F96E4C", machine_store: true, algorithm: "SHA256"
  #end
  if ENV['SIGN_PFX']
    signing_identity_file "#{ENV['SIGN_PFX']}", password: "#{ENV['SIGN_PFX_PW']}", algorithm: "SHA256"
  end
  include_sysprobe = "false"
  if not windows_arch_i386? and ENV['WINDOWS_DDNPM_DRIVER'] and not ENV['WINDOWS_DDNPM_DRIVER'].empty?
    include_sysprobe = "true"
    additional_sign_files_list << "#{Omnibus::Config.source_dir()}\\datadog-agent\\src\\github.com\\DataDog\\datadog-agent\\bin\\agent\\system-probe.exe"
  end
  additional_sign_files additional_sign_files_list
  parameters({
    'InstallDir' => install_dir,
    'InstallFiles' => "#{Omnibus::Config.source_dir()}/datadog-agent/dd-agent/packaging/datadog-agent/win32/install_files",
    'BinFiles' => "#{Omnibus::Config.source_dir()}/datadog-agent/src/github.com/DataDog/datadog-agent/bin/agent",
    'EtcFiles' => "#{Omnibus::Config.source_dir()}\\etc\\datadog-agent",
    'IncludePython2' => "#{with_python_runtime? '2'}",
    'IncludePython3' => "#{with_python_runtime? '3'}",
    'Platform' => "#{arch}",
    'IncludeSysprobe' => "#{include_sysprobe}",
  })
  # This block runs before harvesting with heat.exe
  # It runs in the scope of the packager, so all variables access are from the point-of-view of the packager.
  # Therefore, `install_dir` does not refer to the `install_dir` of the Project but that of the Packager.
  pre_heat do
    def generate_embedded_archive(version)
      safe_embedded_path = windows_safe_path(install_dir, "embedded#{version}")
      safe_embedded_archive_path = windows_safe_path(install_dir, "embedded#{version}.7z")

      shellout!(
        <<-EOH.strip
          7z a -mx=5 -ms=on #{safe_embedded_archive_path} #{safe_embedded_path}
      EOH
      )
      FileUtils.rm_rf "#{safe_embedded_path}"
    end

    # Create the embedded zips and delete their folders
    if File.exist?(windows_safe_path(install_dir, "embedded3"))
      generate_embedded_archive(3)
    end

    if File.exist?(windows_safe_path(install_dir, "embedded2"))
      generate_embedded_archive(2)
    end
  end
end

# ------------------------------------
# Dependencies
# ------------------------------------

# creates required build directories
dependency 'datadog-agent-prepare'

# Linux-specific dependencies
if linux?
  dependency 'procps-ng'
  dependency 'curl'
end

# Datadog agent
dependency 'datadog-agent'

# System-probe
if linux?
  dependency 'system-probe'
end

# Additional software
if windows?
  if ENV['WINDOWS_DDNPM_DRIVER'] and not ENV['WINDOWS_DDNPM_DRIVER'].empty?
    dependency 'datadog-windows-filter-driver'
  end
end
# Bundled cacerts file (is this a good idea?)
dependency 'cacerts'

if osx?
  dependency 'datadog-agent-mac-app'
end

if with_python_runtime? "2"
  dependency 'pylint2'
  dependency 'datadog-agent-integrations-py2'
end

if with_python_runtime? "3"
  dependency 'datadog-agent-integrations-py3'
end

if linux?
  dependency 'datadog-security-agent-policies'
end

# External agents
dependency 'jmxfetch'

# version manifest file
dependency 'version-manifest'

# this dependency puts few files out of the omnibus install dir and move them
# in the final destination. This way such files will be listed in the packages
# manifest and owned by the package manager. This is the only point in the build
# process where we operate outside the omnibus install dir, thus the need of
# the `extra_package_file` directive.
# This must be the last dependency in the project.
dependency 'datadog-agent-finalize'
dependency 'datadog-cf-finalize'

if linux?
  extra_package_file '/etc/init/datadog-agent.conf'
  extra_package_file '/etc/init/datadog-agent-process.conf'
  extra_package_file '/etc/init/datadog-agent-sysprobe.conf'
  extra_package_file '/etc/init/datadog-agent-trace.conf'
  extra_package_file '/etc/init/datadog-agent-security.conf'
  systemd_directory = "/usr/lib/systemd/system"
  if debian?
    systemd_directory = "/lib/systemd/system"

    extra_package_file "/etc/init.d/datadog-agent"
    extra_package_file "/etc/init.d/datadog-agent-process"
    extra_package_file "/etc/init.d/datadog-agent-trace"
    extra_package_file "/etc/init.d/datadog-agent-security"
  end
  extra_package_file "#{systemd_directory}/datadog-agent.service"
  extra_package_file "#{systemd_directory}/datadog-agent-process.service"
  extra_package_file "#{systemd_directory}/datadog-agent-sysprobe.service"
  extra_package_file "#{systemd_directory}/datadog-agent-trace.service"
  extra_package_file "#{systemd_directory}/datadog-agent-security.service"
  extra_package_file '/etc/datadog-agent/'
  extra_package_file '/usr/bin/dd-agent'
  extra_package_file '/var/log/datadog/'
end

# default package_scripts_path and resource_path are based on project name,
# but we change the name based on flavor, so let's hardcode it to "agent"
package_scripts_path "#{Omnibus::Config.project_root}/package-scripts/agent"
resources_path "#{Omnibus::Config.project_root}/resources/agent"

exclude '\.git*'
exclude 'bundler\/git'

if windows?
  #
  # For Windows build, files need to be stripped must be specified here.
  #
  windows_symbol_stripping_file "#{Omnibus::Config.source_dir()}\\cf-root\\bin\\agent\\security-agent.exe"
  windows_symbol_stripping_file "#{Omnibus::Config.source_dir()}\\cf-root\\bin\\agent\\process-agent.exe"
  windows_symbol_stripping_file "#{Omnibus::Config.source_dir()}\\cf-root\\bin\\agent\\trace-agent.exe"
  windows_symbol_stripping_file "#{Omnibus::Config.source_dir()}\\cf-root\\bin\\agent.exe"
  windows_symbol_stripping_file "#{Omnibus::Config.source_dir()}\\datadog-agent\\src\\github.com\\DataDog\\datadog-agent\\bin\\agent\\security-agent.exe"
  windows_symbol_stripping_file "#{Omnibus::Config.source_dir()}\\datadog-agent\\src\\github.com\\DataDog\\datadog-agent\\bin\\agent\\process-agent.exe"
  windows_symbol_stripping_file "#{Omnibus::Config.source_dir()}\\datadog-agent\\src\\github.com\\DataDog\\datadog-agent\\bin\\agent\\trace-agent.exe"
  windows_symbol_stripping_file "#{Omnibus::Config.source_dir()}\\datadog-agent\\src\\github.com\\DataDog\\datadog-agent\\bin\\agent\\agent.exe"
end

if linux? or windows?
  # the stripper will drop the symbols in a `.debug` folder in the installdir
  # we want to make sure that directory is not in the main build, while present
  # in the debug package.
  strip_build true
  debug_path ".debug"  # the strip symbols will be in here
end
